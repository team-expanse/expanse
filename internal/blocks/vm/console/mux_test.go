package console

import (
	"bytes"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a bytes.Buffer safe for the pump and the test to share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// serve starts m on a fresh socket and returns a dialer for clients.
func serve(t *testing.T, m *Mux) func() net.Conn {
	t.Helper()
	path := filepath.Join(t.TempDir(), "console.sock")
	ln, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = m.Serve(ln) }()
	return func() net.Conn {
		conn, err := net.Dial("unix", path)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
}

// readUntil reads conn until it has seen want or the deadline passes.
func readUntil(t *testing.T, conn net.Conn, want string) string {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var got []byte
	buf := make([]byte, 1024)
	for !strings.Contains(string(got), want) {
		n, err := conn.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			t.Fatalf("read %q so far, want %q: %v", got, want, err)
		}
	}
	return string(got)
}

func TestOutputReachesTheLogAndAnAttachedClient(t *testing.T) {
	log := &syncBuffer{}
	m := NewMux(io.Discard, log)
	dial := serve(t, m)
	conn := dial()
	eventually(t, "the client to attach", func() bool { return m.clients() == 1 })

	out, w := io.Pipe()
	go func() { _ = m.Pump(out) }()
	_, _ = w.Write([]byte("login: "))

	readUntil(t, conn, "login: ")
	eventually(t, "the log to get the output", func() bool { return log.String() == "login: " })
}

func TestALateClientFirstGetsRecentOutput(t *testing.T) {
	m := NewMux(io.Discard, io.Discard)
	out, w := io.Pipe()
	go func() { _ = m.Pump(out) }()
	_, _ = w.Write([]byte("booted\r\nlogin: "))
	eventually(t, "the output to be kept", func() bool { return strings.Contains(string(m.recent()), "login: ") })

	conn := serve(t, m)()
	readUntil(t, conn, "booted\r\nlogin: ")
}

func TestHistoryKeepsOnlyTheNewestBytes(t *testing.T) {
	m := NewMux(io.Discard, io.Discard)
	m.publish(bytes.Repeat([]byte("a"), historySize))
	m.publish([]byte("tail"))
	h := m.recent()
	if len(h) != historySize || !bytes.HasSuffix(h, []byte("tail")) {
		t.Fatalf("history is %d bytes ending %q, want %d ending \"tail\"", len(h), h[len(h)-8:], historySize)
	}
}

func TestEveryClientsInputReachesTheGuest(t *testing.T) {
	in := &syncBuffer{}
	m := NewMux(in, io.Discard)
	dial := serve(t, m)
	a, b := dial(), dial()
	_, _ = a.Write([]byte("root\r"))
	eventually(t, "a's input", func() bool { return in.String() == "root\r" })
	_, _ = b.Write([]byte("ls\r"))
	eventually(t, "b's input", func() bool { return in.String() == "root\rls\r" })
}

// A client that stops reading is dropped rather than holding up the guest's output.
func TestAStalledClientIsDroppedAndOutputFlows(t *testing.T) {
	log := &syncBuffer{}
	m := NewMux(io.Discard, log)
	dial := serve(t, m)
	_ = dial() // never reads
	eventually(t, "the client to attach", func() bool { return m.clients() == 1 })

	chunk := bytes.Repeat([]byte("x"), 4096)
	done := make(chan struct{})
	go func() {
		for range 4 * clientBuffer {
			m.publish(chunk)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("output blocked on a client that does not read")
	}
	eventually(t, "the stalled client to be dropped", func() bool { return m.clients() == 0 })
	if got, want := len(log.String()), 4*clientBuffer*len(chunk); got != want {
		t.Errorf("log got %d bytes, want %d", got, want)
	}
}

func TestListenReplacesAStaleSocketAndIsRootOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.sock")
	first, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	first.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = first.Close()
	ln, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen over a stale socket: %v", err)
	}
	defer ln.Close()
	if mode := fileMode(t, path); mode != 0o600 {
		t.Errorf("socket mode %o, want 600", mode)
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	return fi.Mode().Perm()
}
