package recovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	expb "github.com/expanse/expanse/proto"

	"github.com/expanse/expanse/internal/cluster/lease"
	"github.com/expanse/expanse/internal/storage/exvol/localwrite"
	"github.com/expanse/expanse/internal/storage/exvol/primary"
	"github.com/expanse/expanse/internal/storage/exvol/protocol"
	"github.com/expanse/expanse/internal/storage/exvol/secondary"
	"github.com/expanse/expanse/internal/storage/exvol/transport"
	"github.com/expanse/expanse/internal/store/boltstore"
)

// node is one volume replica on the real stack: localwrite on a temp
// file, a real secondary, and a real TCP transport server.
type node struct {
	id   string
	w    *localwrite.Writer
	sec  *secondary.Secondary
	srv  *transport.Server
	addr string
}

var nodeSeq int

func newNode(t *testing.T, id string, size int64) *node {
	t.Helper()
	w, err := localwrite.Open(filepath.Join(t.TempDir(), "zvol"), size)
	if err != nil {
		// O_DIRECT unavailable on this FS: fall back to buffered.
		w, err = localwrite.OpenBuffered(filepath.Join(t.TempDir(), "zvol"), size)
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { w.Close() })
	sec := secondary.New(id, int(size), w)
	sec.SetReader(w)
	ln := portListener()
	srv := transport.NewServer(ln, func(volID string) (transport.Handler, error) {
		return sec.Handler(), nil
	}, nil, 0)
	go srv.Serve() //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	n := &node{id: id, w: w, sec: sec, srv: srv, addr: ln.Addr().String()}
	n.wireRecoveryHandlers()
	return n
}

// wireRecoveryHandlers registers the T11 recovery handlers on the
// node's transport server: seq probes (4a) and durable op fetches
// (4b/4c) served from the local replica.
func (n *node) wireRecoveryHandlers() {
	n.srv.SetQueryHandler(func(volID string) (*expb.SeqQueryReply, error) {
		rep := &expb.SeqQueryReply{VolId: volID, LastSeq: n.sec.LastSeq()}
		for seq, rec := range n.sec.OpLog() {
			rep.Ops = append(rep.Ops, &expb.SeqInfo{Seq: seq, Crc32C: rec.CRC})
		}
		return rep, nil
	})
	n.srv.SetFetchHandler(func(volID string, from, to uint64) (*expb.FetchOpsReply, error) {
		ops, err := n.sec.FetchOps(from, to)
		if err != nil {
			return nil, err
		}
		out := &expb.FetchOpsReply{}
		for _, op := range ops {
			out.Ops = append(out.Ops, &expb.WriteRequest{
				VolId: volID, Seq: op.Seq, Offset: op.Offset,
				Data: op.Data, Crc32C: op.CRC, Flush: op.Flush,
			})
		}
		return out, nil
	})
}

// probe opens a real TCP connection to the node and gathers its 4a
// state.
func (n *node) probe(t *testing.T, volID string, reachable bool) Probe {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !reachable {
		return Probe{NodeID: n.id, Reachable: false}
	}
	conn, err := transport.Dial(ctx, n.addr)
	if err != nil {
		t.Fatalf("probe %s: %v", n.id, err)
	}
	t.Cleanup(func() { conn.Close() })
	rep, err := conn.QuerySeq(volID)
	if err != nil {
		t.Fatalf("probe %s: %v", n.id, err)
	}
	crcs := map[uint64]uint32{}
	for _, op := range rep.GetOps() {
		crcs[op.GetSeq()] = op.GetCrc32C()
	}
	addr := n.addr
	return Probe{
		NodeID:    n.id,
		Reachable: true,
		LastSeq:   rep.GetLastSeq(),
		CRCs:      crcs,
		FetchOps: func(ctx context.Context, from, to uint64) ([]protocol.WriteOp, error) {
			cn, err := transport.Dial(ctx, addr)
			if err != nil {
				return nil, err
			}
			defer cn.Close()
			frep, err := cn.FetchOps(volID, from, to)
			if err != nil {
				return nil, err
			}
			var out []protocol.WriteOp
			for _, req := range frep.GetOps() {
				out = append(out, protocol.WriteOp{
					Seq: req.GetSeq(), Offset: req.GetOffset(),
					Data: req.GetData(), CRC: req.GetCrc32C(), Flush: req.GetFlush(),
				})
			}
			return out, nil
		},
	}
}

// sendOp delivers one op to the node over the real transport.
func (n *node) sendOp(ctx context.Context, volID string, op protocol.WriteOp) error {
	conn, err := transport.Dial(ctx, n.addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.Send(volID, op); err != nil {
		return err
	}
	rep, err := conn.Recv()
	if err != nil {
		return err
	}
	if !rep.ACK {
		return fmt.Errorf("op %d nacked: %s", op.Seq, rep.Reason)
	}
	return nil
}

// --- helpers shared with the wire encoding ---

func mkop(t *testing.T, seq uint64, off uint64, data []byte) protocol.WriteOp {
	t.Helper()
	return protocol.WriteOp{Seq: seq, Offset: off, Data: data, CRC: crc32cOf(data)}
}

func crc32cOf(b []byte) uint32 {
	return crc32.Checksum(b, crc32.MakeTable(crc32.Castagnoli))
}

// readLocal re-reads the node's durable copy for assertions.
func (n *node) readLocal(t *testing.T, off, size int64) []byte {
	t.Helper()
	buf := make([]byte, size)
	if _, err := n.w.ReadAt(buf, off); err != nil {
		t.Fatalf("read local: %v", err)
	}
	return buf
}

var mu sync.Mutex // guards portListener

func portListener() net.Listener {
	mu.Lock()
	defer mu.Unlock()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	return ln
}

// TestFailover101211 runs the spec's exact scenario (§4.3 failover,
// the 10/12/11 case) through the REAL stack: real TCP transport, real
// localwrite replicas, real lease manager (internal/cluster/lease) —
//
//	n1 (old primary, died):      lastSeq 10
//	n2 (candidate, acked extra): lastSeq 12   ← elected (highest)
//	n3 (candidate, acked extra): lastSeq 11
//
// Recovery must elect n2, pull nothing (n2 has the max), sync n3 with
// op 12 (4c), and resume at seq 12 (step 5).
func TestFailover101211(t *testing.T) {
	const volID = "vol-failover-101211"
	const sz = int64(1 << 20)
	ctx := context.Background()

	n1 := newNode(t, "n1", sz)
	n2 := newNode(t, "n2", sz)
	n3 := newNode(t, "n3", sz)

	// Real lease manager over a real store; n2 wins the lease after
	// n1 "dies".
	st, err := boltstore.New(filepath.Join(t.TempDir(), "cluster.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	lm2 := lease.NewManager(st, "n2")
	held, err := lm2.TryAcquire(ctx, "exvol-vol-"+volID, 10*time.Second)
	if err != nil {
		t.Fatalf("candidate could not acquire the volume lease: %v", err)
	}
	defer lm2.Release(ctx, held)

	// Phase 1: n1 was primary, wrote 10 quorum-durable ops.
	payload := bytes.Repeat([]byte{0xA5}, 1024)
	ops := make([]protocol.WriteOp, 0, 12)
	for seq := uint64(1); seq <= 10; seq++ {
		ops = append(ops, mkop(t, seq, (seq-1)*1024, payload))
	}
	for _, op := range ops[:10] {
		// Apply to all three replicas (quorum-durable under n1).
		for _, n := range []*node{n1, n2, n3} {
			if rep := n.sec.Handle(op); !rep.ACK {
				t.Fatalf("setup: replica %s nacked op %d: %s", n.id, op.Seq, rep.Reason)
			}
		}
	}

	// Phase 2: the 4b scenario — ops durable on secondaries that the
	// dying primary never counted: op 11 on n2+n3, op 12 on n2 only.
	op11 := mkop(t, 11, 10*1024, bytes.Repeat([]byte{0x11}, 1024))
	op12 := mkop(t, 12, 11*1024, bytes.Repeat([]byte{0x22}, 1024))
	for _, n := range []*node{n2, n3} {
		if rep := n.sec.Handle(op11); !rep.ACK {
			t.Fatalf("setup: nacked 11 on %s", n.id)
		}
	}
	if rep := n2.sec.Handle(op12); !rep.ACK {
		t.Fatalf("setup: nacked 12 on n2")
	}

	// n1 is DEAD — its probe is unreachable (steps 4a/4d).
	probes := []Probe{
		{NodeID: "n1", Reachable: false},
		n2.probe(t, volID, true),
		n3.probe(t, volID, true),
	}

	var sent []protocol.WriteOp
	res, err := Recover(ctx, probes,
		n2.probe(t, volID, true).FetchOps, // the caller's own durable copy (caller = n2 here)
		nil,                               // new primary needs no pulls in this scenario
		func(_ context.Context, target string, op protocol.WriteOp) error {
			sent = append(sent, op)
			// Deliver over the real transport to the real secondary.
			dst := map[string]*node{"n2": n2, "n3": n3}[target]
			return dst.sendOp(ctx, volID, op)
		})
	if err != nil {
		t.Fatal(err)
	}
	if res.NewPrimaryID != "n2" {
		t.Fatalf("elected %q, want n2 (highest lastSeq)", res.NewPrimaryID)
	}
	if res.MaxSeq != 12 {
		t.Fatalf("MaxSeq = %d, want 12", res.MaxSeq)
	}
	if len(res.Stale) != 1 || res.Stale[0] != "n1" {
		t.Fatalf("Stale = %v, want [n1]", res.Stale)
	}
	if res.Pulled != 0 {
		t.Fatalf("Pulled = %d, want 0 (n2 already had the max)", res.Pulled)
	}
	if res.Synced["n3"] != 1 {
		t.Fatalf("Synced = %v, want n3:1 (op 12 only)", res.Synced)
	}

	// Step 4c verified on the real replica: n3 now durable at 12 with
	// the exact bytes op 12 carried.
	if got := n3.sec.LastSeq(); got != 12 {
		t.Fatalf("n3 lastSeq = %d, want 12", got)
	}
	if !bytes.Equal(n3.readLocal(t, 11*1024, 1024), op12.Data) {
		t.Fatal("n3's op-12 bytes do not match what n2 held")
	}
	if n3.sec.LastSeq() != 12 || n2.sec.LastSeq() != 12 {
		t.Fatal("replicas not level at seq 12")
	}

	// Step 5: the new primary resumes AT MaxSeq — the coordinator must
	// never restart the logical counter at 0 (R2). Constructed against
	// the REAL acquired lease.
	coord := primary.NewAt(volID, 3, n2.w, nil, held, time.Second, res.MaxSeq)
	if coord.LastSeq() != 12 {
		t.Fatalf("recovered coordinator starts at seq %d, want 12", coord.LastSeq())
	}
}

// TestFailoverPullBack verifies step 4b end-to-end: the new primary is
// BEHIND a reachable replica and must pull the missing op before
// serving — without it, an op acked by 2 of 3 but not counted by the
// dying primary would be lost.
func TestFailoverPullBack(t *testing.T) {
	const volID = "vol-failover-pull"
	const sz = int64(1 << 20)
	ctx := context.Background()

	na := newNode(t, "n1", sz) // candidate: lastSeq 10
	nb := newNode(t, "nb", sz) // holds op 11 the candidate lacks
	nc := newNode(t, "nc", sz)

	for seq := uint64(1); seq <= 10; seq++ {
		op := mkop(t, seq, (seq-1)*1024, bytes.Repeat([]byte{0x5A}, 1024))
		for _, n := range []*node{na, nb, nc} {
			n.sec.Handle(op)
		}
	}
	op11 := mkop(t, 11, 10*1024, bytes.Repeat([]byte{0xBB}, 1024))
	if rep := nc.sec.Handle(op11); !rep.ACK {
		t.Fatal("setup: nacked 11 on nc")
	}

	// Candidate na's local writer is its durable copy; Recover's apply
	// callback writes the pulled op into it (the real new primary's
	// local replica).
	var pulled []protocol.WriteOp
	dst := map[string]*node{"n1": na, "nb": nb, "nc": nc}
	res, err := Recover(ctx, []Probe{
		na.probe(t, volID, true),
		nb.probe(t, volID, true),
		nc.probe(t, volID, true),
	},
		nc.probe(t, volID, true).FetchOps, // the caller's own durable copy (caller = nc)
		func(_ context.Context, op protocol.WriteOp) error {
			pulled = append(pulled, op)
			return na.w.WriteAt(op.Data, int64(op.Offset))
		},
		func(_ context.Context, target string, op protocol.WriteOp) error {
			return dst[target].sendOp(ctx, volID, op)
		})
	if err != nil {
		t.Fatal(err)
	}
	if res.NewPrimaryID != "nc" {
		t.Fatalf("elected %q, want nc (lastSeq 11)", res.NewPrimaryID)
	}
	if res.Pulled != 0 {
		t.Fatalf("Pulled = %d, want 0 (nc holds the max itself)", res.Pulled)
	}
	// nb and na are both behind; both must be brought to 11.
	if res.Synced["nb"] != 1 || res.Synced["n1"] != 1 {
		t.Fatalf("Synced = %v, want nb:1 n1:1", res.Synced)
	}
	if len(pulled) != 0 {
		t.Fatalf("pulled %v, want none", pulled)
	}
	// na received op 11 over the real transport.
	if got := na.sec.LastSeq(); got != 11 {
		t.Fatalf("na lastSeq = %d, want 11", got)
	}
	if !bytes.Equal(na.readLocal(t, 10*1024, 1024), op11.Data) {
		t.Fatal("na's op-11 bytes do not match")
	}
}

// TestDivergenceRefused is the §9 split-brain rule: the same seq with
// different CRCs on two reachable replicas must refuse automatic
// recovery (NeedsManualRecovery) and PRESERVE all copies — no apply,
// no send, no delete.
func TestDivergenceRefused(t *testing.T) {
	const volID = "vol-diverged"
	const sz = int64(1 << 20)
	ctx := context.Background()

	n1 := newNode(t, "n1", sz)
	n2 := newNode(t, "n2", sz)

	// Both replicas durable at seq 3 — with DIFFERENT content (a
	// split-brain split: two primaries each acked "seq 3" to a
	// different replica). Note the secondary treats a same-seq op as an
	// idempotent duplicate, so the branches must be built op-by-op.
	good := mkop(t, 3, 2*1024, bytes.Repeat([]byte{0x01}, 1024))
	bad := protocol.WriteOp{Seq: 3, Offset: 2 * 1024, Data: bytes.Repeat([]byte{0x02}, 1024)}
	bad.CRC = crc32cOf(bad.Data) // valid CRC, different branch
	for seq := uint64(1); seq <= 2; seq++ {
		op := mkop(t, seq, (seq-1)*1024, bytes.Repeat([]byte{0x01}, 1024))
		for _, n := range []*node{n1, n2} {
			n.sec.Handle(op)
		}
	}
	n1.sec.Handle(good)
	n2.sec.Handle(bad)

	var applied, sent []protocol.WriteOp
	_, err := Recover(ctx, []Probe{
		n1.probe(t, volID, true),
		n2.probe(t, volID, true),
	},
		nil, // divergence guard fires before any data movement
		func(_ context.Context, op protocol.WriteOp) error {
			applied = append(applied, op)
			return nil
		},
		func(_ context.Context, _ string, op protocol.WriteOp) error {
			sent = append(sent, op)
			return nil
		})
	var div *DivergedError
	if !errors.As(err, &div) {
		t.Fatalf("want DivergedError, got %v", err)
	}
	if !div.NeedsManualRecovery() {
		t.Fatal("divergence must demand manual recovery")
	}
	if len(div.Divergences) != 1 || div.Divergences[0].Seq != 3 {
		t.Fatalf("divergences = %+v, want seq 3", div.Divergences)
	}
	// All copies preserved: no data movement whatsoever.
	if len(applied) != 0 || len(sent) != 0 {
		t.Fatalf("recovery moved data on a diverged volume (applied %d, sent %d)", len(applied), len(sent))
	}
	// Both replicas still hold their (conflicting) copies untouched.
	if n1.sec.LastSeq() != 3 || n2.sec.LastSeq() != 3 {
		t.Fatal("diverged replicas were modified")
	}
}
