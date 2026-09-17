package primary

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/storage/exvol/localwrite"
	"github.com/expanse/expanse/internal/storage/exvol/protocol"
	"github.com/expanse/expanse/internal/storage/exvol/transport"
)

// fakeLease is a scriptable validity gate (production passes
// *lease.Held, whose Valid() has the same shape).
type fakeLease struct{ valid bool }

func (f *fakeLease) Valid() bool { return f.valid }

// testReplica is one secondary: local file (T06 Writer semantics via a
// plain buffer), T04 protocol secondary, T05 server on loopback.
type testReplica struct {
	id    string
	dir   string
	data  []byte
	sec   *protocol.Secondary
	srv   *transport.Server
	addr  string
	size  int64
	mu    sync.Mutex
	delay time.Duration // handler stall to simulate a slow replica
}

func newTestReplica(t *testing.T, id string, size int64) *testReplica {
	t.Helper()
	r := &testReplica{id: id, size: size, data: make([]byte, size)}
	r.sec = protocol.NewSecondary(id, int(size))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r.srv = transport.NewServer(ln, func(volID string) (transport.Handler, error) {
		return func(op protocol.WriteOp) protocol.Reply {
			r.mu.Lock()
			d := r.delay
			r.mu.Unlock()
			if d > 0 {
				time.Sleep(d)
			}
			copy(r.data[op.Offset:], op.Data)
			return r.sec.Handle(op)
		}, nil
	}, nil, 0)
	go r.srv.Serve() //nolint:errcheck — test server
	t.Cleanup(func() { r.srv.Close() })
	return r
}

func (r *testReplica) connect(t *testing.T) *transport.Sender {
	t.Helper()
	c, err := transport.Dial(context.Background(), r.addrOrPanic())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return transport.NewSender(c, 0, 0)
}

func (r *testReplica) addrOrPanic() string {
	addr := r.srv.Addr().String()
	if addr == "" {
		panic("no addr")
	}
	_ = addr
	return r.srv.Addr().String()
}

// newTestPrimary builds a coordinator over real transport connections
// to the given replicas, with a local file as the primary's own zvol.
func newTestPrimary(t *testing.T, size int64, repl []*testReplica, l Lease, timeout time.Duration) (*Coordinator, *localwrite.Writer, []*transport.Conn) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "primary.zvol")
	w, err := localwrite.Open(path, size)
	if err != nil {
		w, err = localwrite.OpenBuffered(path, size)
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { w.Close() })
	var reps []Replica
	var conns []*transport.Conn
	for _, r := range repl {
		cn, err := transport.Dial(context.Background(), r.srv.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cn.Close() })
		conns = append(conns, cn)
		reps = append(reps, Replica{NodeID: r.id, Sender: transport.NewSender(cn, 0, 0)})
	}
	return New("vol-test", len(repl)+1, w, reps, l, timeout), w, conns
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}

// --- Tests ---

func TestWriteQuorumAckWithSlowThird(t *testing.T) {
	size := int64(1 << 20)
	n1 := newTestReplica(t, "n1", size)
	n2 := newTestReplica(t, "n2", size)
	n3 := newTestReplica(t, "n3", size)
	n3.mu.Lock()
	n3.delay = 1500 * time.Millisecond // slower than the stale timeout, not dead
	n3.mu.Unlock()

	c, _, _ := newTestPrimary(t, size, []*testReplica{n1, n2, n3}, &fakeLease{valid: true}, 500*time.Millisecond)

	start := time.Now()
	if err := c.Write([]byte("hello durable world"), 100); err != nil {
		t.Fatalf("write with 1 slow replica: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("write took %v — slow replica blocked quorum ack (§9 violation)", elapsed)
	}

	// Quorum replicas (n1, n2) have the data; primary too.
	want := []byte("hello durable world")
	if !bytes.Equal(n1.data[100:100+len(want)], want) {
		t.Fatal("n1 did not apply the write")
	}
	if !bytes.Equal(n2.data[100:100+len(want)], want) {
		t.Fatal("n2 did not apply the write")
	}

	// Subsequent writes still succeed fast: the slow replica (still
	// mid-op when the quorum ack returned) gets marked Stale instead of
	// blocking further writes (§9).
	start = time.Now()
	for i := 0; i < 5; i++ {
		if err := c.Write([]byte(fmt.Sprintf("subsequent-%d", i)), int64(200+i*32)); err != nil {
			t.Fatalf("write %d after stale mark: %v", i, err)
		}
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("5 subsequent writes took %v total — slow replica still blocking", elapsed)
	}
}

// With only one secondary, quorum cannot be reached without it — the
// deadline must mark it Stale instead of blocking forever, and
// subsequent writes must fail fast rather than re-block.
func TestSlowReplicaMarkedStaleAtDeadline(t *testing.T) {
	size := int64(1 << 20)
	n1 := newTestReplica(t, "n1", size)
	n1.mu.Lock()
	n1.delay = 1500 * time.Millisecond // slower than the 500ms stale timeout
	n1.mu.Unlock()
	c, _, _ := newTestPrimary(t, size, []*testReplica{n1}, &fakeLease{valid: true}, 500*time.Millisecond)

	start := time.Now()
	err := c.Write([]byte("slow"), 0)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("write with unreachable quorum accepted")
	}
	if elapsed > 1200*time.Millisecond {
		t.Fatalf("write took %v — stalled well past the stale timeout", elapsed)
	}
	if len(c.StaleReplicas()) != 1 {
		t.Fatalf("slow replica not marked stale: %v", c.StaleReplicas())
	}
	start = time.Now()
	if err := c.Write([]byte("fast fail"), 32); err == nil {
		t.Fatal("write with all secondaries stale accepted")
	}
	if elapsed = time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("write after stale mark took %v — must fail fast, not re-wait", elapsed)
	}
}

func TestWriteDeadReplicaDoesNotBlock(t *testing.T) {
	size := int64(1 << 20)
	n1 := newTestReplica(t, "n1", size)
	dead := newTestReplica(t, "dead", size)
	c, _, conns := newTestPrimary(t, size, []*testReplica{n1, dead}, &fakeLease{valid: true}, 500*time.Millisecond)

	// Kill the dead replica before the first write: closing only the
	// listener leaves accepted conns serving, so drop the client conn
	// too (node death takes both).
	dead.srv.Close()
	conns[1].Close()

	start := time.Now()
	if err := c.Write([]byte("survives"), 0); err != nil {
		t.Fatalf("write with dead replica: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("write took %v — dead replica blocked ack", elapsed)
	}
	waitFor(t, func() bool { return len(c.StaleReplicas()) == 1 }, "dead replica not marked stale")
}

func TestLeaseInvalidImmediateEIONoWrite(t *testing.T) {
	size := int64(1 << 20)
	n1 := newTestReplica(t, "n1", size)
	c, local, _ := newTestPrimary(t, size, []*testReplica{n1}, &fakeLease{valid: false}, time.Second)

	start := time.Now()
	err := c.Write([]byte("must not persist"), 0)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("write with invalid lease accepted")
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("lease-invalid write took %v — must be immediate EIO, no queueing", elapsed)
	}
	if c.LastSeq() != 0 {
		t.Fatalf("seq assigned despite invalid lease (%d)", c.LastSeq())
	}
	// Nothing written locally or remotely.
	probe := make([]byte, 16)
	local.ReadAt(probe, 0) //nolint:errcheck
	if !bytes.Equal(probe, make([]byte, 16)) {
		t.Error("local replica written despite invalid lease")
	}
	if !bytes.Equal(n1.data[:16], make([]byte, 16)) {
		t.Error("secondary written despite invalid lease")
	}
}

func TestWriteConvergesAllReplicas(t *testing.T) {
	size := int64(1 << 20)
	reps := []*testReplica{newTestReplica(t, "n1", size), newTestReplica(t, "n2", size), newTestReplica(t, "n3", size)}
	c, local, _ := newTestPrimary(t, size, reps, &fakeLease{valid: true}, 2*time.Second)

	for i := 0; i < 20; i++ {
		payload := bytes.Repeat([]byte{byte(i + 1)}, 100+i)
		if err := c.Write(payload, int64(i)*500+int64(i)); err != nil { // unaligned offsets
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if got := c.LastSeq(); got != 20 {
		t.Fatalf("LastSeq = %d, want 20", got)
	}
	// Every replica byte-identical to the primary on the written
	// regions (protocol R4: same ops, same order → same bytes).
	ref := make([]byte, size)
	if _, err := local.ReadAt(ref, 0); err != nil && err.Error() != "EOF" {
		t.Fatal(err)
	}
	check := func() bool {
		for _, r := range reps {
			for i := 0; i < 20; i++ {
				off := int64(i)*500 + int64(i)
				n := 100 + i
				if !bytes.Equal(r.data[off:off+int64(n)], ref[off:off+int64(n)]) {
					return false
				}
			}
		}
		return true
	}
	// Quorum can return before the slowest replica applies — poll for
	// convergence rather than assuming immediate.
	waitFor(t, check, "replicas did not converge with the primary")
}

func TestAllReplicasDeadReturnsUnavailable(t *testing.T) {
	size := int64(1 << 20)
	n1 := newTestReplica(t, "n1", size)
	n2 := newTestReplica(t, "n2", size)
	c, _, conns := newTestPrimary(t, size, []*testReplica{n1, n2}, &fakeLease{valid: true}, 300*time.Millisecond)
	n1.srv.Close()
	n2.srv.Close()
	conns[0].Close() // simulate node death: the server-side conns stay serving after listener close
	conns[1].Close()
	if err := c.Write([]byte("x"), 0); err == nil {
		t.Fatal("write with all replicas dead accepted")
	}
}
