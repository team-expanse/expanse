package vip

import (
	"context"
	"io"
	"net"
	"sync"
	"time"

	"github.com/expanse/expanse/internal/errors"
)

// TCPProxy is the minimal L4 listener behind a VIP: bind VIP:port,
// splice each accepted connection to a backend address (the local
// replica's target port). T10–T12 replace this with the full L4/L7
// load balancers; this exists so a VIP is actually servable the
// moment it is held.
type TCPProxy struct {
	// Target is the backend "host:port" dialed per connection.
	Target string
	// DialTimeout bounds backend connects.
	DialTimeout time.Duration

	mu     sync.Mutex
	lis    net.Listener
	conns  map[net.Conn]struct{}
	closed bool
	wg     sync.WaitGroup
}

// ListenOn binds addr (VIP:port), starts the accept loop in a
// goroutine, and returns the listener as an io.Closer (the holder's
// listener seam). The returned closer stops accepting AND closes all
// live connections.
func (p *TCPProxy) ListenOn(addr string) (io.Closer, error) {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, errors.Wrap(err, errors.KindUnavailable, "vip.proxy", "listen "+addr)
	}
	p.mu.Lock()
	if p.conns == nil {
		p.conns = make(map[net.Conn]struct{})
	}
	p.lis = lis
	p.mu.Unlock()
	go p.accept(lis)
	return stopper{p, lis}, nil
}

type stopper struct {
	p   *TCPProxy
	lis net.Listener
}

// Close implements the shutdown order INSIDE the proxy: stop
// accepting, then drop live connections. The §4.2 holder sequence
// (AddrDel → listener.Close → ConnTracker.CloseAll) wraps this.
func (s stopper) Close() error {
	s.p.mu.Lock()
	s.p.closed = true
	lis := s.p.lis
	conns := make([]net.Conn, 0, len(s.p.conns))
	for c := range s.p.conns {
		conns = append(conns, c)
	}
	s.p.mu.Unlock()
	if lis != nil {
		_ = lis.Close()
	}
	for _, c := range conns {
		_ = c.Close()
	}
	return nil
}

func (p *TCPProxy) accept(lis net.Listener) {
	for {
		c, err := lis.Accept()
		if err != nil {
			return // listener closed or transient; holder re-listens on re-acquisition
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			_ = c.Close()
			return
		}
		p.conns[c] = struct{}{}
		p.mu.Unlock()
		p.wg.Add(1)
		go p.serve(c)
	}
}

func (p *TCPProxy) serve(c net.Conn) {
	defer p.wg.Done()
	defer func() {
		p.mu.Lock()
		delete(p.conns, c)
		p.mu.Unlock()
		_ = c.Close()
	}()

	d := p.DialTimeout
	if d <= 0 {
		d = 3 * time.Second
	}
	// Deadline-aware backend dial; on failure the client connection
	// closes and the client retries (the VIP may be about to move).
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	b, derr := (&net.Dialer{}).DialContext(ctx, "tcp", p.Target)
	if derr != nil {
		return
	}
	defer func() { _ = b.Close() }()

	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(b, c); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, b); done <- struct{}{} }()
	<-done // first direction to finish ends the splice
}

// CloseAll implements ConnTracker: drop every live connection.
func (p *TCPProxy) CloseAll() {
	p.mu.Lock()
	conns := make([]net.Conn, 0, len(p.conns))
	for c := range p.conns {
		conns = append(conns, c)
	}
	p.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}
