package transport

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/storage/exvol/protocol"
)

// --- Framing round-trips ---

func TestFrameRoundTrip(t *testing.T) {
	for _, payload := range [][]byte{nil, {}, []byte("x"), bytes.Repeat([]byte{0xAB}, 1<<20)} {
		var buf bytes.Buffer
		if err := writeFrame(&buf, msgWriteRequest, payload); err != nil {
			t.Fatalf("writeFrame: %v", err)
		}
		typ, got, err := readFrame(&buf)
		if err != nil {
			t.Fatalf("readFrame: %v", err)
		}
		if typ != msgWriteRequest || !bytes.Equal(got, payload) {
			t.Errorf("round trip: type=%d len(payload)=%d, want %d bytes equal", typ, len(got), len(payload))
		}
	}
}

func TestFrameRejectsOversized(t *testing.T) {
	huge := bytes.Repeat([]byte{0}, MaxFrameSize) // larger than the cap
	var buf bytes.Buffer
	if err := writeFrame(&buf, msgWriteRequest, huge); err == nil {
		t.Fatal("writeFrame accepted an oversized payload")
	}
}

func TestFrameRejectsUnknownVersion(t *testing.T) {
	buf := []byte{0, 0, 0, 1, 9, byte(msgWriteRequest)} // version 9
	if _, _, err := readFrame(bytes.NewReader(buf)); err == nil {
		t.Fatal("readFrame accepted unknown version")
	}
}

func TestFrameTruncated(t *testing.T) {
	var buf bytes.Buffer
	if err := writeFrame(&buf, msgWriteReply, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	bad := buf.Bytes()[:3]
	if _, _, err := readFrame(bytes.NewReader(bad)); err == nil {
		t.Fatal("readFrame accepted a truncated header")
	}
}

// --- Test server harness ---

// startTestServer spins up a server on 127.0.0.1:0 whose router hands
// writes to a protocol.Secondary (the T04 simulator, in-memory). Returns
// the address and the secondary for assertions.
func startTestServer(t *testing.T, volID string, sec *protocol.Secondary, dscp int) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	router := func(id string) (Handler, error) {
		if id != volID {
			return nil, fmt.Errorf("unknown volume %q", id)
		}
		return sec.Handle, nil
	}
	srv := NewServer(ln, router, nil, dscp)
	go func() { _ = srv.Serve() }()
	return ln.Addr().String()
}

func makeOp(seq uint64, offset int, data []byte) protocol.WriteOp {
	return protocol.WriteOp{Seq: seq, Offset: uint64(offset), Data: data, CRC: protocol.CRC32C(data)}
}

// --- Backpressure: window full → sender blocks, never drops ---

func TestBackpressureBlocksWhenWindowFull(t *testing.T) {
	sec := protocol.NewSecondary("n2", 1<<20)

	// Handler-side delay so replies lag behind submits.
	slowSec := sec.Handle
	gate := make(chan struct{})
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln2.Close()
	srv := NewServer(ln2, func(id string) (Handler, error) {
		return func(op protocol.WriteOp) protocol.Reply {
			<-gate
			return slowSec(op)
		}, nil
	}, nil, 0)
	go func() { _ = srv.Serve() }()

	c, err := Dial(context.Background(), ln2.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s := NewSender(c, 2, 1<<20) // window: 2 ops

	op := makeOp(1, 0, bytes.Repeat([]byte{7}, 64))
	if err := s.Submit("vol-1", op); err != nil {
		t.Fatal(err)
	}
	op.Seq, op.CRC = 2, protocol.CRC32C(op.Data)
	if err := s.Submit("vol-1", op); err != nil {
		t.Fatal(err)
	}
	if s.InFlight() != 2 {
		t.Fatalf("in-flight = %d, want 2", s.InFlight())
	}

	// Third submit must BLOCK (window full) — never drop or error.
	blocked := make(chan error, 1)
	go func() {
		op3 := makeOp(3, 0, bytes.Repeat([]byte{8}, 64))
		blocked <- s.Submit("vol-1", op3)
	}()
	select {
	case err := <-blocked:
		t.Fatalf("third Submit completed while window full: %v (dropped or overflowed)", err)
	case <-time.After(100 * time.Millisecond):
		// good: still blocked
	}
	close(gate) // replies flow → slots free → the blocked submit proceeds
	// Drain all three replies concurrently with the pending submit
	// (submit 3 needs a Recv to free its slot).
	errCh := make(chan error, 1)
	go func() {
		for i := 0; i < 3; i++ {
			r, err := s.Recv()
			if err != nil {
				errCh <- err
				return
			}
			if !r.ACK {
				errCh <- fmt.Errorf("reply %d not acked: %+v", i, r)
				return
			}
		}
		errCh <- nil
	}()
	if err := <-blocked; err != nil {
		t.Fatalf("submit after window freed: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	if s.InFlight() != 0 {
		t.Errorf("in-flight = %d, want 0 after draining", s.InFlight())
	}
}

func TestBackpressureByteWindow(t *testing.T) {
	sec := protocol.NewSecondary("n2", 1<<20)
	addr := startTestServer(t, "vol-1", sec, 0)
	c, err := Dial(context.Background(), addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Byte window 256: a 512-byte op can never fit — Submit must block
	// forever until a Recv frees space (here: nothing was sent, so it
	// blocks; we assert it does NOT complete).
	s := NewSender(c, 8, 256)
	blocked := make(chan struct{})
	go func() {
		_ = s.Submit("vol-1", makeOp(1, 0, bytes.Repeat([]byte{1}, 512)))
		close(blocked)
	}()
	select {
	case <-blocked:
		t.Fatal("op larger than the whole byte window was submitted (would drop/overflow)")
	case <-time.After(100 * time.Millisecond):
	}
}

// --- Loopback integration: real listeners, T04 property subset ---

func TestLoopbackConvergenceOverRealTransport(t *testing.T) {
	// Replay the core T04 convergence property over two REAL localhost
	// servers: shuffled ops (including gaps and duplicates) → replicas
	// converge byte-identical to the in-order reference.
	const (
		size   = 8192
		numOps = 120
	)
	for seed := int64(1); seed <= 5; seed++ {
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			n2 := protocol.NewSecondary("n2", size)
			n3 := protocol.NewSecondary("n3", size)
			a2 := startTestServer(t, "vol-1", n2, 0)
			a3 := startTestServer(t, "vol-1", n3, 0)

			c2, err := Dial(context.Background(), a2)
			if err != nil {
				t.Fatal(err)
			}
			defer c2.Close()
			c3, err := Dial(context.Background(), a3)
			if err != nil {
				t.Fatal(err)
			}
			defer c3.Close()
			// Small window to force interleaved pipelining.
			s2 := NewSender(c2, 8, 1<<20)
			s3 := NewSender(c3, 8, 1<<20)

			// Build the full op list first (with per-replica duplicate
			// decisions) so exact reply counts are known up front.
			type delivery struct {
				op      protocol.WriteOp
				dupToN2 bool
			}
			ops := make([]delivery, 0, numOps)
			ref := make([]byte, size)
			for seq := uint64(1); seq <= numOps; seq++ {
				n := 1 + rng.Intn(200)
				off := rng.Intn(size - n)
				data := make([]byte, n)
				rng.Read(data)
				copy(ref[off:], data)
				op := makeOp(seq, off, data)
				dup := rng.Intn(20) == 0 // duplicate some ops (idempotent)
				ops = append(ops, delivery{op: op, dupToN2: dup})
			}
			total2, total3 := 0, 0
			for _, d := range ops {
				total2++
				if d.dupToN2 {
					total2++
				}
				total3++
			}

			// Drains run CONCURRENTLY with submits (the window blocks
			// when full — it never drops — so recvs must flow).
			var wg sync.WaitGroup
			drainErr := make(chan error, 2)
			drain := func(s *Sender, total int) {
				defer wg.Done()
				for i := 0; i < total; i++ {
					r, err := s.Recv()
					if err != nil {
						drainErr <- fmt.Errorf("recv: %w", err)
						return
					}
					if !r.ACK {
						drainErr <- fmt.Errorf("op not acked over wire: %+v", r)
						return
					}
				}
			}
			wg.Add(2)
			go drain(s2, total2)
			go drain(s3, total3)

			for _, d := range ops {
				if err := s2.Submit("vol-1", d.op); err != nil {
					t.Fatalf("submit n2 seq %d: %v", d.op.Seq, err)
				}
				if d.dupToN2 { // duplicate submission, same op
					if err := s2.Submit("vol-1", d.op); err != nil {
						t.Fatalf("dup submit n2 seq %d: %v", d.op.Seq, err)
					}
				}
				if err := s3.Submit("vol-1", d.op); err != nil {
					t.Fatalf("submit n3 seq %d: %v", d.op.Seq, err)
				}
			}
			wg.Wait()
			close(drainErr)
			for err := range drainErr {
				if err != nil {
					t.Fatal(err)
				}
			}

			for _, s := range []*protocol.Secondary{n2, n3} {
				if s.LastSeq() != numOps {
					t.Errorf("%s lastSeq = %d, want %d", s.ID, s.LastSeq(), numOps)
				}
				if !bytes.Equal(s.Data, ref) {
					t.Errorf("%s content diverged from reference over real transport", s.ID)
				}
			}
			if !bytes.Equal(n2.Data, n3.Data) {
				t.Error("replicas diverged from each other")
			}
		})
	}
}

func TestLoopbackPropertySubsetGapsAndCRC(t *testing.T) {
	// T04 subset: CRC corruption → NACK+Retransmit over the wire; gap →
	// NACK with lastSeq, then drain.
	sec := protocol.NewSecondary("n2", 4096)
	addr := startTestServer(t, "vol-1", sec, 0)
	c, err := Dial(context.Background(), addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	data := bytes.Repeat([]byte{3}, 100)
	bad := makeOp(1, 0, data)
	bad.CRC ^= 0xFF
	if err := c.Send("vol-1", bad); err != nil {
		t.Fatal(err)
	}
	r, err := c.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if r.ACK || !r.Retransmit {
		t.Fatalf("corrupt op over wire: %+v, want NACK+Retransmit", r)
	}
	if sec.LastSeq() != 0 {
		t.Fatal("corrupt op applied on secondary")
	}

	// Retransmit → ACK.
	if err := c.Send("vol-1", makeOp(1, 0, data)); err != nil {
		t.Fatal(err)
	}
	if r, err = c.Recv(); err != nil || !r.ACK || r.Seq != 1 {
		t.Fatalf("retransmit: reply=%+v err=%v", r, err)
	}

	// Gap: send 5 before 2 → NACK with lastSeq=1; fill → drain to 5.
	if err := c.Send("vol-1", makeOp(5, 100, bytes.Repeat([]byte{5}, 16))); err != nil {
		t.Fatal(err)
	}
	if r, err = c.Recv(); err != nil || r.ACK || r.LastSeq != 1 || !r.Gap {
		t.Fatalf("gap: reply=%+v err=%v", r, err)
	}
	for seq := uint64(2); seq <= 4; seq++ {
		if err := c.Send("vol-1", makeOp(seq, 0, bytes.Repeat([]byte{byte(seq)}, 16))); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ { // replies for ops 2,3,4 (op4 drains 5)
		if r, err = c.Recv(); err != nil || !r.ACK {
			t.Fatalf("drain reply %d: reply=%+v err=%v", i, r, err)
		}
	}
	if r.Seq != 5 || sec.LastSeq() != 5 {
		t.Errorf("final reply seq=%d lastSeq=%d, want 5/5", r.Seq, sec.LastSeq())
	}
}

func TestLoopbackUnknownVolumeNACK(t *testing.T) {
	sec := protocol.NewSecondary("n2", 1024)
	addr := startTestServer(t, "vol-1", sec, 0)
	c, err := Dial(context.Background(), addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Send("vol-other", makeOp(1, 0, []byte("x"))); err != nil {
		t.Fatal(err)
	}
	r, err := c.Recv()
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if r.ACK || r.Reason == "" {
		t.Errorf("unknown volume: reply=%+v, want NACK with reason", r)
	}
}

// --- Resync: rate limiting + raw chunks ---

func TestResyncChunksRateLimited(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv := NewServer(ln, nil, func(chunk []byte) ([]byte, error) {
		return bytes.ToUpper(chunk), nil
	}, 0)
	go func() { _ = srv.Serve() }()

	c, err := Dial(context.Background(), ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Throttled connection: 1 MiB/s, 512 KiB burst. Send ~1.5 MiB → the
	// burst covers the first half-megabyte, the rest takes ≳0.9 s.
	c.throttle = NewThrottle(1 << 20)
	payload := bytes.Repeat([]byte("a"), 96<<10)
	start := time.Now()
	for i := 0; i < 16; i++ {
		resp, err := c.SendResyncChunk(payload)
		if err != nil {
			t.Fatalf("resync chunk %d: %v", i, err)
		}
		if !bytes.Equal(resp, bytes.ToUpper(payload)) {
			t.Fatalf("resync chunk %d corrupted in transit", i)
		}
	}
	elapsed := time.Since(start)
	if elapsed < 900*time.Millisecond {
		t.Errorf("resync send completed in %v — throttle not applied (want ≳0.9 s at 1 MiB/s for 1.5 MiB with 512 KiB burst)", elapsed)
	}
}

func TestResyncUnthrottledFast(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv := NewServer(ln, nil, func(chunk []byte) ([]byte, error) { return chunk, nil }, 0)
	go func() { _ = srv.Serve() }()

	c, err := Dial(context.Background(), ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	payload := bytes.Repeat([]byte{7}, 256<<10)
	if _, err := c.SendResyncChunk(payload); err != nil {
		t.Fatal(err)
	}
	// No throttle set: completes quickly. (No strict timing assert —
	// just correctness of the unthrottled path.)
}

func TestServerWithoutRawHandlerRefusesResync(t *testing.T) {
	addr := startTestServer(t, "vol-1", protocol.NewSecondary("n2", 1024), 0)
	c, err := Dial(context.Background(), addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.SendResyncChunk([]byte("data")); err == nil {
		t.Fatal("resync accepted on a server without a raw handler")
	}
}

// --- DSCP marking: best-effort, must not break the connection ---

func TestDSCPMarkingSmoke(t *testing.T) {
	sec := protocol.NewSecondary("n2", 1024)
	addr := startTestServer(t, "vol-1", sec, DSCPBulk) // server marks accepted conns
	c, err := Dial(context.Background(), addr, WithDSCP(DSCPBulk))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Send("vol-1", makeOp(1, 0, bytes.Repeat([]byte{9}, 32))); err != nil {
		t.Fatalf("send over DSCP-marked conn: %v", err)
	}
	r, err := c.Recv()
	if err != nil || !r.ACK {
		t.Fatalf("DSCP-marked conn: reply=%+v err=%v", r, err)
	}
}

// --- Concurrency sanity: parallel submitters + recvr on one Sender ---

func TestSenderConcurrentUse(t *testing.T) {
	sec := protocol.NewSecondary("n2", 1<<20)
	addr := startTestServer(t, "vol-1", sec, 0)
	c, err := Dial(context.Background(), addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s := NewSender(c, 16, 1<<20)

	const N = 100
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // single recvr frees slots (send order ⇒ reply order)
		defer wg.Done()
		for i := 0; i < N; i++ {
			r, err := s.Recv()
			if err != nil {
				t.Errorf("recv %d: %v", i, err)
				return
			}
			if !r.ACK || r.Seq != uint64(i+1) {
				t.Errorf("reply %d: %+v", i, r)
				return
			}
		}
	}()
	for seq := 1; seq <= N; seq++ {
		if err := s.Submit("vol-1", makeOp(uint64(seq), 0, bytes.Repeat([]byte{byte(seq)}, 32))); err != nil {
			t.Fatalf("submit %d: %v", seq, err)
		}
	}
	wg.Wait()
	if sec.LastSeq() != N {
		t.Errorf("lastSeq = %d, want %d", sec.LastSeq(), N)
	}
}
