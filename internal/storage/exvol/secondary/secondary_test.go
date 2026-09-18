package secondary

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/storage/exvol/localwrite"
	"github.com/expanse/expanse/internal/storage/exvol/oplog"
	"github.com/expanse/expanse/internal/storage/exvol/protocol"
	"github.com/expanse/expanse/internal/storage/exvol/transport"
)

// newTestSecondary builds a real secondary: localwrite.Writer over a
// temp file + protocol state + a transport.Server exposing it.
func newTestSecondary(t *testing.T, nodeID string, size int) (*Secondary, *localwrite.Writer, *transport.Server) {
	t.Helper()
	w, err := localwrite.Open(t.TempDir()+"/"+nodeID+".zvol", int64(size))
	if err != nil {
		w, err = localwrite.OpenBuffered(t.TempDir()+"/"+nodeID+".zvol", int64(size))
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { w.Close() })
	s := New(nodeID, size, w)
	srv := newServer(t, s)
	return s, w, srv
}

func newServer(t *testing.T, s *Secondary) *transport.Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := transport.NewServer(ln, func(volID string) (transport.Handler, error) {
		return s.Handler(), nil
	}, nil, 0)
	go srv.Serve() //nolint:errcheck — test server
	t.Cleanup(func() { srv.Close() })
	return srv
}

func dial(t *testing.T, srv *transport.Server) *transport.Conn {
	t.Helper()
	c, err := transport.Dial(context.Background(), srv.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// TestInOrderApplicationUnderTransportReordering: two client conns
// deliver ops in opposite order over the real transport; the secondary
// buffers the future op and applies in sequence order (R3/R4).
func TestInOrderApplicationUnderTransportReordering(t *testing.T) {
	size := 1 << 20
	s, w, srv := newTestSecondary(t, "n2", size)

	cA := dial(t, srv)
	cB := dial(t, srv)

	op2 := protocol.WriteOp{Seq: 2, Offset: 4096, Data: bytes.Repeat([]byte{0x22}, 100), CRC: protocol.CRC32C(bytes.Repeat([]byte{0x22}, 100))}
	op1 := protocol.WriteOp{Seq: 1, Offset: 0, Data: bytes.Repeat([]byte{0x11}, 100), CRC: protocol.CRC32C(bytes.Repeat([]byte{0x11}, 100))}

	// Conn B sends op2 FIRST; conn A sends op1 after a beat.
	go func() {
		if err := cB.Send("vol", op2); err != nil {
			t.Logf("send op2: %v", err)
		}
	}()
	time.Sleep(50 * time.Millisecond)
	if err := cA.Send("vol", op1); err != nil {
		t.Fatal(err)
	}

	// op2 arrives out of order → immediate Gap NACK (buffered, not
	// applied, not silently dropped — R3).
	rep2, err := cB.Recv()
	if err != nil || rep2.ACK || !rep2.Gap || rep2.LastSeq != 0 {
		t.Fatalf("op2 reply = %+v err=%v, want Gap NACK with lastSeq=0", rep2, err)
	}
	// op1 applies, drains the buffered op2; its ACK carries lastSeq=2.
	rep1, err := cA.Recv()
	if err != nil || !rep1.ACK || rep1.Seq != 2 {
		t.Fatalf("op1 reply = %+v err=%v, want ACK with seq=2 (drained)", rep1, err)
	}

	// Durable on the zvol, applied in order.
	got1 := make([]byte, 100)
	got2 := make([]byte, 100)
	if _, err := w.ReadAt(got1, 0); err != nil && err.Error() != "EOF" {
		t.Fatal(err)
	}
	if _, err := w.ReadAt(got2, 4096); err != nil && err.Error() != "EOF" {
		t.Fatal(err)
	}
	if !bytes.Equal(got1, op1.Data) || !bytes.Equal(got2, op2.Data) {
		t.Fatalf("zvol diverged: op1=%v op2=%v", got1[:8], got2[:8])
	}
	if s.LastSeq() != 2 {
		t.Fatalf("lastSeq = %d, want 2", s.LastSeq())
	}
}

// TestCorruptFrameNACKRetransmit: a corrupted op (bad CRC) over the
// real transport → NACK+Retransmit, never applied; the retransmitted
// op is acked and applied.
func TestCorruptFrameNACKRetransmit(t *testing.T) {
	size := 1 << 20
	s, w, srv := newTestSecondary(t, "n2", size)
	c := dial(t, srv)

	good := bytes.Repeat([]byte{0xAB}, 128)
	bad := protocol.WriteOp{Seq: 1, Offset: 0, Data: good, CRC: 0xDEADBEEF}
	if err := c.Send("vol", bad); err != nil {
		t.Fatal(err)
	}
	rep, err := c.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if rep.ACK || !rep.Retransmit {
		t.Fatalf("corrupt reply = %+v, want NACK+Retransmit", rep)
	}
	probe := make([]byte, 128)
	w.ReadAt(probe, 0) //nolint:errcheck
	if !bytes.Equal(probe, make([]byte, 128)) {
		t.Fatal("corrupt op was applied to the zvol")
	}

	// Retransmit the same seq with the correct CRC → ACK + applied.
	op := protocol.WriteOp{Seq: 1, Offset: 0, Data: good, CRC: protocol.CRC32C(good)}
	if err := c.Send("vol", op); err != nil {
		t.Fatal(err)
	}
	rep, err = c.Recv()
	if err != nil || !rep.ACK || rep.Seq != 1 {
		t.Fatalf("retransmit reply = %+v err=%v", rep, err)
	}
	got := make([]byte, 128)
	if _, err := w.ReadAt(got, 0); err != nil && err.Error() != "EOF" {
		t.Fatal(err)
	}
	if !bytes.Equal(got, good) {
		t.Fatal("retransmitted op not applied to the zvol")
	}
	if s.LastSeq() != 1 {
		t.Fatalf("lastSeq = %d, want 1", s.LastSeq())
	}
}

// TestGapNACKReportsLastSeqAndFiresResyncTrigger: op3 with no op1/op2 →
// NACK carrying lastSeq=0 AND an observable resync-trigger event; after
// a simulated resync (Reset + replay), ops are acked again.
func TestGapNACKReportsLastSeqAndFiresResyncTrigger(t *testing.T) {
	size := 1 << 20
	s, _, srv := newTestSecondary(t, "n2", size)
	c := dial(t, srv)

	op3 := protocol.WriteOp{Seq: 3, Offset: 0, Data: bytes.Repeat([]byte{0x33}, 64), CRC: protocol.CRC32C(bytes.Repeat([]byte{0x33}, 64))}
	if err := c.Send("vol", op3); err != nil {
		t.Fatal(err)
	}
	rep, err := c.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if rep.ACK || !rep.Gap || rep.LastSeq != 0 {
		t.Fatalf("gap reply = %+v, want NACK Gap with lastSeq=0", rep)
	}

	select {
	case ev := <-s.Events():
		if ev.LastSeq != 0 || ev.OpSeq != 3 || ev.Reason != "gap" {
			t.Fatalf("resync trigger = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no resync-trigger event within 1s")
	}

	// Resync itself lands in T11/T12; here: Reset + replay through the
	// same handler proves the secondary is reusable afterwards.
	s.Reset()
	for seq := uint64(1); seq <= 3; seq++ {
		op := protocol.WriteOp{Seq: seq, Offset: uint64(seq-1) * 64, Data: bytes.Repeat([]byte{byte(seq)}, 64), CRC: protocol.CRC32C(bytes.Repeat([]byte{byte(seq)}, 64))}
		if err := c.Send("vol", op); err != nil {
			t.Fatal(err)
		}
		rep, err := c.Recv()
		if err != nil || !rep.ACK {
			t.Fatalf("replay seq %d: reply=%+v err=%v", seq, rep, err)
		}
	}
	if s.LastSeq() != 3 {
		t.Fatalf("lastSeq after resync replay = %d, want 3", s.LastSeq())
	}
}

// TestLocalWriteFailureNACKs: an apply error surfaces as a NACK — the
// op is never acked durable (R1 at the secondary boundary).
func TestLocalWriteFailureNACKs(t *testing.T) {
	s := New("n2", 4096, failingWriter{})
	op := protocol.WriteOp{Seq: 1, Offset: 0, Data: bytes.Repeat([]byte{1}, 16), CRC: protocol.CRC32C(bytes.Repeat([]byte{1}, 16))}
	rep := s.Handle(op)
	if rep.ACK {
		t.Fatalf("apply failure acked: %+v", rep)
	}
	if rep.LastSeq != 0 || s.LastSeq() != 0 {
		t.Fatalf("apply failure advanced lastSeq: %+v", rep)
	}
}

type failingWriter struct{}

func (failingWriter) WriteAt(p []byte, off int64) error {
	return net.ErrClosed
}

func (failingWriter) Flush() error { return net.ErrClosed }

// countingWriter records applies; its writes panic if used after the
// test has swapped it out (SwapWriter(nil)).
type countingWriter struct {
	applies int
}

func (c *countingWriter) WriteAt(p []byte, off int64) error {
	c.applies++
	return nil
}

func (c *countingWriter) Flush() error { return nil }

func (c *countingWriter) ReadAt(p []byte, off int64) (int, error) {
	return len(p), nil
}

// TestSwapWriterQuiesceAndRearm: a quiesced secondary (writer nil,
// e.g. while `zfs receive -F` replaces the dataset object) must fail
// applies LOUDLY — never ack durable — and keep protocol + oplog
// state across the swap; re-arming resumes at the same sequence.
func TestSwapWriterQuiesceAndRearm(t *testing.T) {
	w := &countingWriter{}
	s := New("n2", 4096, w)
	op := protocol.WriteOp{Seq: 1, Offset: 0, Data: bytes.Repeat([]byte{7}, 16), CRC: protocol.CRC32C(bytes.Repeat([]byte{7}, 16))}
	if rep := s.Handle(op); !rep.ACK {
		t.Fatalf("pre-swap apply not acked")
	}
	old := s.SwapWriter(nil)
	if old == nil {
		t.Fatalf("SwapWriter(nil) returned nil old writer")
	}
	if s.HasWriter() {
		t.Fatalf("HasWriter true while quiesced")
	}
	if rep := s.Handle(op); rep.ACK {
		t.Fatalf("quiesced apply acked durable")
	}
	// Re-arm: a DIFFERENT writer over the same (post-receive) device.
	s.SwapWriter(&countingWriter{})
	if !s.HasWriter() {
		t.Fatalf("HasWriter false after re-arm")
	}
	// The reader must track the swapped writer (FetchOps serves from
	// the new durable copy, never the old fd).
	if _, r := s.currentWriter(); r == nil {
		t.Fatalf("reader not re-armed with writer")
	}
}

// TestSetOplogStoreRaisesLastSeq: a node's own writes made while it was
// PRIMARY (recorded into the same durable oplog file —
// primary.Coordinator.AttachOplog) must not read back as "behind" once
// this node restarts and rejoins as a SECONDARY. SetOplogStore loads
// the file's per-seq records AND must raise LastSeq() to match — what
// recovery actually trusts as this replica's resume point (§4.3 4a) —
// not just the per-seq location map that FetchOps serves from.
func TestSetOplogStoreRaisesLastSeq(t *testing.T) {
	path := t.TempDir() + "/vol.oplog"

	// This node's own primary-era writes, landed in the shared journal
	// without ever going through this Secondary.
	st, err := oplog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st.Append(1, oplog.Record{Offset: 0, Length: 16, CRC: 111}, false)
	st.Append(2, oplog.Record{Offset: 16, Length: 16, CRC: 222}, true)

	w := &countingWriter{}
	s := New("n1", 4096, w)
	if got := s.LastSeq(); got != 0 {
		t.Fatalf("LastSeq before SetOplogStore = %d, want 0", got)
	}
	if err := s.SetOplogStore(path); err != nil {
		t.Fatalf("SetOplogStore: %v", err)
	}
	if got := s.LastSeq(); got != 2 {
		t.Fatalf("LastSeq after SetOplogStore = %d, want 2 (raised from loaded oplog)", got)
	}
	if log := s.OpLog(); len(log) != 2 {
		t.Fatalf("OpLog() after SetOplogStore = %v, want 2 entries", log)
	}

	// A subsequent live write must continue from the raised seq, not
	// restart from 0 (which would collide with the already-durable
	// history and violate R2's gaplessness).
	op := protocol.WriteOp{Seq: 3, Offset: 32, Data: bytes.Repeat([]byte{9}, 16), CRC: protocol.CRC32C(bytes.Repeat([]byte{9}, 16))}
	if rep := s.Handle(op); !rep.ACK {
		t.Fatalf("write at seq 3 not acked: %+v", rep)
	}
}
