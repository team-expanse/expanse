package device

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/storage/exvol/localwrite"
	"github.com/expanse/expanse/internal/storage/exvol/primary"
	"github.com/expanse/expanse/internal/storage/exvol/protocol"
	"github.com/expanse/expanse/internal/storage/exvol/transport"
)

// --- BlockDevice interface semantics against fake backing ---

type fakeLocal struct {
	data     []byte
	failDisc bool
}

func (f *fakeLocal) ReadAt(p []byte, off int64) (int, error) {
	copy(p, f.data[off:off+int64(len(p))])
	return len(p), nil
}
func (f *fakeLocal) WriteAt(p []byte, off int64) error { copy(f.data[off:], p); return nil }
func (f *fakeLocal) Flush() error                      { return nil }
func (f *fakeLocal) Discard(off, length int64) error {
	if f.failDisc {
		return net.ErrClosed
	}
	for i := off; i < off+length; i++ {
		f.data[i] = 0
	}
	return nil
}
func (f *fakeLocal) Close() error { return nil }

// deviceOf builds an ExvolDevice over fakes.
func deviceOf(t *testing.T, size int64, l primary.Lease) *ExvolDevice {
	t.Helper()
	f := &fakeLocal{data: make([]byte, size)}
	return &ExvolDevice{volID: "test-vol", size: size, local: f, coord: nil, lease: l}
}

func TestBlockDeviceReadWriteFlushDiscardSemantics(t *testing.T) {
	dev := deviceOf(t, 4096, nil)

	if dev.Size() != 4096 {
		t.Fatalf("size = %d", dev.Size())
	}
	if n, err := dev.WriteAt([]byte("hello block"), 100); err != nil || n != 11 {
		t.Fatalf("write: %d %v", n, err)
	}
	got := make([]byte, 11)
	if n, err := dev.ReadAt(got, 100); err != nil || n != 11 || !bytes.Equal(got, []byte("hello block")) {
		t.Fatalf("read: %d %v %q", n, err, got)
	}
	if err := dev.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if err := dev.Discard(96, 32); err != nil {
		t.Fatalf("discard: %v", err)
	}
	got = make([]byte, 11)
	dev.ReadAt(got, 100) //nolint:errcheck
	if !bytes.Equal(got, make([]byte, 11)) {
		t.Fatalf("discard did not zero the range: %q", got)
	}
	if err := dev.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := dev.Close(); err != nil { // idempotent
		t.Fatalf("double close: %v", err)
	}
	if _, err := dev.ReadAt(got, 0); err == nil {
		t.Fatal("read after close accepted")
	}
}

func TestBlockDeviceRangeChecks(t *testing.T) {
	dev := deviceOf(t, 4096, nil)
	defer dev.Close()
	if _, err := dev.WriteAt(make([]byte, 16), 4090); err == nil {
		t.Error("write past end accepted")
	}
	if _, err := dev.ReadAt(make([]byte, 16), 4090); err == nil {
		t.Error("read past end accepted")
	}
	if _, err := dev.WriteAt(make([]byte, 16), -1); err == nil {
		t.Error("negative offset accepted")
	}
	if err := dev.Discard(4000, 128); err == nil {
		t.Error("discard past end accepted")
	}
}

// TestLeaseLossEIOPromptNotHanging: after lease loss every op returns
// EIO promptly — the test itself times out loudly if a call blocks
// (§4.4: a hung device makes unkillable D-state processes).
func TestLeaseLossEIOPromptNotHanging(t *testing.T) {
	fl := &fakeLocal{data: make([]byte, 4096)}
	lease := &fakeLease{valid: true}
	dev := &ExvolDevice{volID: "v", size: 4096, local: fl, lease: lease}

	watchdog := time.AfterFunc(2*time.Second, func() {
		panic("device call still blocked after 2s — §4.4 hang, not EIO")
	})
	defer watchdog.Stop()

	lease.valid = false // lose the lease mid-life
	deadline := time.Now().Add(time.Second)

	if _, err := dev.WriteAt([]byte("x"), 0); err == nil || time.Now().After(deadline) {
		t.Fatalf("write: err=%v (must be prompt EIO)", err)
	}
	if _, err := dev.ReadAt(make([]byte, 8), 0); err == nil || time.Now().After(deadline) {
		t.Fatalf("read: err=%v (must be prompt EIO)", err)
	}
	if err := dev.Flush(context.Background()); err == nil || time.Now().After(deadline) {
		t.Fatalf("flush: err=%v (must be prompt EIO)", err)
	}
	if err := dev.Discard(0, 64); err == nil || time.Now().After(deadline) {
		t.Fatalf("discard: err=%v (must be prompt EIO)", err)
	}
}

type fakeLease struct{ valid bool }

func (f *fakeLease) Valid() bool { return f.valid }

// --- ExvolDevice against a REAL coordinator + localwrite + fake replicas ---

func TestExvolDeviceWriteQuorumDurable(t *testing.T) {
	size := int64(1 << 20)
	reps := make([]*fakeSecondary, 2)
	for i := range reps {
		reps[i] = newFakeSecondary(t, size)
	}
	c, local := newCoordinator(t, size, reps)
	lease := &fakeLease{valid: true}
	dev := New("vol-x", size, local, c, lease)

	if _, err := dev.WriteAt([]byte("durable!"), 5000); err != nil {
		t.Fatal(err)
	}
	// Read path comes from the local replica.
	got := make([]byte, 8)
	if n, err := dev.ReadAt(got, 5000); err != nil || n != 8 || !bytes.Equal(got, []byte("durable!")) {
		t.Fatalf("read: %d %v %q", n, err, got)
	}
	for i, r := range reps {
		if !bytes.Equal(r.data()[5000:5008], []byte("durable!")) {
			t.Fatalf("replica %d missing write", i)
		}
	}
	if err := dev.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Discard goes to the primary's local zvol.
	if err := dev.Discard(4000, 2048); err != nil {
		t.Fatal(err)
	}
}

// --- NBD server tests: a Go NBD client over the unix socket ---

func TestNBDRoundTrip(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "vol.nbd.sock")
	srv, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	fl := &fakeLocal{data: make([]byte, 1<<20)}
	srv.SetDevice(&ExvolDevice{volID: "v", size: 1 << 20, local: fl, lease: nil})
	go srv.Serve() //nolint:errcheck

	cl := nbdDial(t, sock, 1<<20)

	// WRITE then READ back.
	if _, err := nbdCommand(cl, nbdCmdWrite, 0, int64(len("nbd payload")), []byte("nbd payload")); err != nil {
		t.Fatal(err)
	}
	got, err := nbdCommand(cl, nbdCmdRead, 0, 11, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte("nbd payload")) {
		t.Fatalf("read = %q", got)
	}
	// FLUSH.
	if _, err := nbdCommand(cl, nbdCmdFlush, 0, 0, nil); err != nil {
		t.Fatal(err)
	}
	// WRITE_ZEROES over the payload range.
	if _, err := nbdCommand(cl, nbdWriteZeroes, 0, 100, nil); err != nil {
		t.Fatal(err)
	}
	got, err = nbdCommand(cl, nbdCmdRead, 0, 11, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, make([]byte, 11)) {
		t.Fatalf("write-zeroes did not zero: %q", got)
	}
	// TRIM.
	if _, err := nbdCommand(cl, nbdCmdTrim, 2000, 100, nil); err != nil {
		t.Fatal(err)
	}
}

func TestNBDErrorReplyOnLeaseLoss(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "vol.nbd.sock")
	srv, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	lease := &fakeLease{valid: true}
	fl := &fakeLocal{data: make([]byte, 1<<20)}
	srv.SetDevice(&ExvolDevice{volID: "v", size: 1 << 20, local: fl, lease: lease})
	go srv.Serve() //nolint:errcheck

	cl := nbdDial(t, sock, 1<<20)
	lease.valid = false

	watchdog := time.AfterFunc(2*time.Second, func() {
		panic("NBD command blocked after lease loss — §4.4 hang")
	})
	defer watchdog.Stop()

	if _, err := nbdCommand(cl, nbdCmdWrite, 0, 4, []byte("nope")); err == nil {
		t.Fatal("write after lease loss accepted")
	}
}

// fakeSecondary is a remote-replica stand-in served over the real
// transport: the coordinator connects to it exactly like a secondary.
type fakeSecondary struct {
	mu    sync.Mutex
	_data []byte
}

func newFakeSecondary(t *testing.T, size int64) *fakeSecondary {
	return &fakeSecondary{_data: make([]byte, size)}
}

func (f *fakeSecondary) data() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f._data
}

func (f *fakeSecondary) handle(op protocol.WriteOp) protocol.Reply {
	if op.Flush {
		return protocol.Reply{ACK: true, Seq: op.Seq}
	}
	copy(f._data[op.Offset:], op.Data)
	return protocol.Reply{ACK: true, Seq: op.Seq}
}

func newCoordinator(t *testing.T, size int64, reps []*fakeSecondary) (*primary.Coordinator, *localwrite.Writer) {
	t.Helper()
	w, err := localwrite.Open(filepath.Join(t.TempDir(), "p.zvol"), size)
	if err != nil {
		w, err = localwrite.OpenBuffered(filepath.Join(t.TempDir(), "p.zvol"), size)
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { w.Close() })
	var prs []primary.Replica
	for i, r := range reps {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		srv := transport.NewServer(ln, func(string) (transport.Handler, error) {
			return r.handle, nil
		}, nil, 0)
		go srv.Serve() //nolint:errcheck
		t.Cleanup(func() { srv.Close() })
		cn, err := transport.Dial(context.Background(), srv.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cn.Close() })
		_ = i
		prs = append(prs, primary.Replica{NodeID: srv.Addr().String(), Sender: transport.NewSender(cn, 0, 0)})
	}
	return primary.New("vol-x", len(reps)+1, w, prs, nil, 2*time.Second), w
}

// --- minimal NBD client for tests ---

type nbdConn struct {
	nc net.Conn
}

func nbdDial(t *testing.T, sock string, size int64) *nbdConn {
	t.Helper()
	nc, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nc.Close() })

	// Handshake.
	hs := make([]byte, 18)
	if _, err := io.ReadFull(nc, hs); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(hs[:8], []byte(nbdMagic)) {
		t.Fatalf("bad server magic: % x", hs[:8])
	}
	if binary.BigEndian.Uint64(hs[8:]) != nbdNewStyle {
		t.Fatalf("expected newstyle: % x", hs[8:16])
	}
	// Client flags.
	if _, err := nc.Write([]byte{0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	// NBD_OPT_EXPORT_NAME with empty export name: IHAVEOPT(8) +
	// option(4) + length(4)=0.
	opt := make([]byte, 16)
	binary.BigEndian.PutUint64(opt[0:], nbdNewStyle)
	binary.BigEndian.PutUint32(opt[8:], nbdOptExport)
	if _, err := nc.Write(opt); err != nil {
		t.Fatal(err)
	}
	info := make([]byte, 134)
	if _, err := io.ReadFull(nc, info); err != nil {
		t.Fatal(err)
	}
	got := binary.BigEndian.Uint64(info[0:])
	if got != uint64(size) {
		t.Fatalf("export size = %d, want %d", got, size)
	}
	return &nbdConn{nc: nc}
}

func nbdCommand(c *nbdConn, cmd uint16, off int64, length int64, data []byte) ([]byte, error) {
	handle := uint64(42)
	req := make([]byte, 28)
	binary.BigEndian.PutUint32(req[0:], nbdReqMagic)
	binary.BigEndian.PutUint16(req[6:], cmd)
	binary.BigEndian.PutUint64(req[8:], handle)
	binary.BigEndian.PutUint64(req[16:], uint64(off))
	binary.BigEndian.PutUint32(req[24:], uint32(length))
	if cmd == nbdCmdWrite {
		req = append(req, data...)
	}
	if _, err := c.nc.Write(req); err != nil {
		return nil, err
	}
	if cmd == nbdCmdDisc {
		return nil, nil
	}
	var rep [16]byte
	if _, err := io.ReadFull(c.nc, rep[:]); err != nil {
		return nil, err
	}
	if binary.BigEndian.Uint32(rep[0:]) != nbdRepMagic {
		return nil, io.ErrUnexpectedEOF
	}
	if e := binary.BigEndian.Uint32(rep[4:]); e != 0 {
		return nil, &nbdError{code: e}
	}
	if cmd == nbdCmdRead {
		buf := make([]byte, length)
		if _, err := io.ReadFull(c.nc, buf); err != nil {
			return nil, err
		}
		return buf, nil
	}
	return nil, nil
}

type nbdError struct{ code uint32 }

func (e *nbdError) Error() string { return "nbd error " + string(rune('0'+e.code)) }
