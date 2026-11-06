// Package console serves a VM's serial console: the guest's output goes to the journal and to
// every attached client, and their input goes to the guest.
package console

import (
	"errors"
	"io"
	"net"
	"os"
	"sync"
)

// SocketPath is the console socket of a VM replica; only root on the node can open it.
func SocketPath(instance string) string {
	return "/run/expblk-" + instance + "-console.sock"
}

const (
	// historySize is how much recent output a newly attached client sees first.
	historySize = 64 << 10
	// clientBuffer is how many chunks a client may fall behind before it is dropped.
	clientBuffer = 256
)

// Mux fans the guest's serial output out to a log and its clients, and their input in.
type Mux struct {
	in  io.Writer
	log io.Writer

	inMu sync.Mutex // one client's write reaches the guest whole

	mu       sync.Mutex
	history  []byte
	attached map[*client]struct{}
}

type client struct {
	conn net.Conn
	out  chan []byte
	once sync.Once
	gone chan struct{}
}

// drop ends the client; closing its connection also frees a write stuck on it.
func (c *client) drop() {
	c.once.Do(func() {
		close(c.gone)
		_ = c.conn.Close()
	})
}

// NewMux writes client input to in and copies all output to log.
func NewMux(in, log io.Writer) *Mux {
	return &Mux{in: in, log: log, attached: map[*client]struct{}{}}
}

// Listen opens a root-only console socket at path, replacing a stale one.
func Listen(path string) (net.Listener, error) {
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

// Pump copies the guest's output until r ends.
func (m *Mux) Pump(r io.Reader) error {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			m.publish(buf[:n])
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// publish logs p, keeps it in the history and queues it for each client; a full queue drops the client.
func (m *Mux) publish(p []byte) {
	_, _ = m.log.Write(p)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.history = append(m.history, p...)
	if over := len(m.history) - historySize; over > 0 {
		m.history = append(m.history[:0:0], m.history[over:]...)
	}
	for c := range m.attached {
		select {
		case c.out <- append([]byte(nil), p...):
		default:
			c.drop()
			delete(m.attached, c)
		}
	}
}

// Serve attaches each connection on ln as a client until ln closes.
func (m *Mux) Serve(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go m.attach(conn)
	}
}

// attach sends conn the recent output, then live output, while relaying its input.
func (m *Mux) attach(conn net.Conn) {
	c := &client{conn: conn, out: make(chan []byte, clientBuffer), gone: make(chan struct{})}
	m.mu.Lock()
	recent := append([]byte(nil), m.history...)
	m.attached[c] = struct{}{}
	m.mu.Unlock()
	defer m.detach(c)

	go m.relayInput(conn, c)
	if _, err := conn.Write(recent); err != nil {
		return
	}
	for {
		select {
		case p := <-c.out:
			if _, err := conn.Write(p); err != nil {
				return
			}
		case <-c.gone:
			return
		}
	}
}

func (m *Mux) relayInput(conn net.Conn, c *client) {
	defer c.drop()
	buf := make([]byte, 1024)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			m.inMu.Lock()
			_, werr := m.in.Write(buf[:n])
			m.inMu.Unlock()
			if werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (m *Mux) detach(c *client) {
	c.drop()
	m.mu.Lock()
	delete(m.attached, c)
	m.mu.Unlock()
}

// clients is how many clients are attached.
func (m *Mux) clients() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.attached)
}

// recent is a copy of the kept output.
func (m *Mux) recent() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]byte(nil), m.history...)
}
