package proxy

import (
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

// BalancerMode selects the backend-selection algorithm (§4.3 L4).
type BalancerMode string

const (
	RoundRobin BalancerMode = "round-robin" // default
	LeastConn  BalancerMode = "least-connections"
	SourceHash BalancerMode = "source-hash" // sticky per client IP
	// PrimaryOnly restricts candidates to Service.Primary() instead of
	// picking among every healthy backend (D2): the right mode for a
	// block whose replicas are not interchangeable, e.g. db/postgres,
	// where a write landing on a standby fails or misroutes. Not a
	// selection algorithm over many backends — the candidate set it
	// narrows to is at most one, so which algorithm picks within it
	// (pickAt's default case) does not matter.
	PrimaryOnly BalancerMode = "primary-only"
)

// Defaults per §4.3.
const (
	DefaultDrainTimeout = 30 * time.Second
	DefaultDialTimeout  = 5 * time.Second

	// L7 response/idle defaults (§4.3).
	DefaultResponseHeaderTimeout = 30 * time.Second
	DefaultIdleTimeout           = 90 * time.Second
)

// dialRetryBackoff is the pause between L4.handle's own dial retries —
// short enough not to matter to a client's own connect-time budget,
// long enough to give a transient "backend briefly too busy to accept()"
// condition a real chance to clear before retrying.
const dialRetryBackoff = 20 * time.Millisecond

// Resolver maps a backend to its dial address ("host:port"). The
// seam isolates the proxy from node addressing (agent wiring resolves
// from the mesh/inventory; tests map backends to local listeners).
type Resolver func(b Backend, targetPort int32) string

// TableSource is the pool surface L4 needs; *Pool satisfies it.
type TableSource interface {
	Table() *Table
}

// L4 is one TCP load-balancer instance: a listener per VIP:port that
// splices client connections to healthy backends from the pool table.
// The table is re-read per accepted connection (fresh snapshot), so a
// backend removed from the pool stops receiving new work immediately;
// its existing connections drain until they finish or DrainTimeout
// elapses, then are force-closed.
type L4 struct {
	Pool    TableSource
	Key     string       // service key "<namespace>/<name>"
	Mode    BalancerMode // default round-robin
	Resolve Resolver     // required
	// TargetPort, when nonzero, is dialed instead of the service's (a block's later VIP ports).
	TargetPort int32
	// DialFrom is the local address to dial b from; the zero Addr lets the kernel pick.
	DialFrom func(b Backend) netip.Addr

	MaxPerBackend int           // 0 = unlimited
	DrainTimeout  time.Duration // default 30 s
	DialTimeout   time.Duration // default 5 s

	rr     atomic.Uint64
	active sync.Map // backendID → *atomic.Int64
	conns  sync.Map // backendID → *connSet

	mu     sync.Mutex
	closed bool
}

type connSet struct {
	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

func (l *L4) backendID(b Backend) string {
	return fmt.Sprintf("%s/%d", b.NodeID, b.ReplicaIndex)
}

func (l *L4) incActive(id string) *atomic.Int64 {
	v, _ := l.active.LoadOrStore(id, &atomic.Int64{})
	p := v.(*atomic.Int64)
	p.Add(1)
	return p
}

func (l *L4) registerConn(id string, c net.Conn) {
	v, _ := l.conns.LoadOrStore(id, &connSet{conns: map[net.Conn]struct{}{}})
	s := v.(*connSet)
	s.mu.Lock()
	s.conns[c] = struct{}{}
	s.mu.Unlock()
}

func (l *L4) unregisterConn(id string, c net.Conn) {
	if v, ok := l.conns.Load(id); ok {
		s := v.(*connSet)
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
	}
}

func (l *L4) closeBackendConns(id string) {
	if v, ok := l.conns.Load(id); ok {
		s := v.(*connSet)
		s.mu.Lock()
		for c := range s.conns {
			_ = c.Close()
		}
		s.conns = map[net.Conn]struct{}{}
		s.mu.Unlock()
	}
}

// acceptRetryCap bounds Serve's own Accept() backoff (mirrors
// net/http.Server.Serve's long-established pattern for exactly this
// class of error).
const acceptRetryCap = time.Second

// Serve runs the accept loop until ln is closed or ctx is canceled. A
// transient Accept() error (a momentary FD/resource limit, not this
// listener being intentionally closed) retries with a capped backoff
// instead of returning outright — found running the Stream D
// vertical-slice VM test: a sustained burst of short-lived connections
// eventually hit exactly this, and with no retry here, the WHOLE VIP's
// listener died silently for the rest of its holder's tenure (every
// later connection attempt simply went nowhere), since nothing else
// ever re-invokes Serve short of a full, much rarer VIP handover
// (internal/agent's own lbListen only logs the error and returns).
// Blocks; run in a goroutine.
func (l *L4) Serve(ctx context.Context, ln net.Listener) error {
	go l.drainLoop(ctx)
	var retryDelay time.Duration
	for {
		c, err := ln.Accept()
		if err != nil {
			l.mu.Lock()
			closed := l.closed
			l.mu.Unlock()
			if closed {
				return nil
			}
			if retryDelay == 0 {
				retryDelay = 5 * time.Millisecond
			} else if retryDelay *= 2; retryDelay > acceptRetryCap {
				retryDelay = acceptRetryCap
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(retryDelay):
			}
			continue
		}
		retryDelay = 0
		go l.handle(c)
	}
}

// Close stops accepting new connections (the listener must be closed
// by its owner); established connections finish or drain out.
func (l *L4) Close() {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
}

// drainLoop enforces the drain invariant: a backend removed from the
// pool finishes its existing connections but never keeps them longer
// than DrainTimeout, after which they are force-closed.
func (l *L4) drainLoop(ctx context.Context) {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	missing := map[string]time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		tab := l.Pool.Table()
		svc := tab.Service(l.Key)
		present := map[string]bool{}
		if svc != nil {
			for _, b := range svc.Backends {
				present[l.backendID(b)] = true
			}
		}
		l.conns.Range(func(k, v any) bool {
			id := k.(string)
			s := v.(*connSet)
			s.mu.Lock()
			n := len(s.conns)
			s.mu.Unlock()
			if n == 0 || present[id] {
				delete(missing, id)
				return true
			}
			since, ok := missing[id]
			if !ok {
				missing[id] = time.Now()
				return true
			}
			if time.Since(since) >= l.drainTimeout() {
				l.closeBackendConns(id)
				delete(missing, id)
			}
			return true
		})
	}
}

func (l *L4) drainTimeout() time.Duration {
	if l.DrainTimeout > 0 {
		return l.DrainTimeout
	}
	return DefaultDrainTimeout
}

func (l *L4) mode() BalancerMode {
	if l.Mode == "" {
		return RoundRobin
	}
	return l.Mode
}

func (l *L4) dialTimeout() time.Duration {
	if l.DialTimeout > 0 {
		return l.DialTimeout
	}
	return DefaultDialTimeout
}

func (l *L4) activeCount(id string) int64 {
	if v, ok := l.active.Load(id); ok {
		return v.(*atomic.Int64).Load()
	}
	return 0
}

// handle splices one client connection to the chosen backend. Connect
// errors retry against the SAME candidate set (connection establishment
// is side-effect free — §9 D5.7's "connect errors only, idempotent
// operations" rule applied to L4 dials), not a shrinking one that
// permanently drops a candidate after a single failed dial: found
// running the Stream D vertical-slice VM test, PrimaryOnly's own
// candidate set is never more than one backend (by design — D2), so
// the old shrink-on-failure logic gave a transient dial hiccup against
// the ONLY valid destination (postgres's own accept() queue briefly
// busy under a bursty synchronous-commit-heavy write workload, not the
// backend actually being down) zero retry margin — the client's
// connection was simply closed outright on the very first transient
// failure, no different backend ever existed to fall back to. A short
// pause between attempts gives a genuinely transient condition a chance
// to clear; a truly dead backend still fails every attempt, just
// costing a bounded few dial timeouts instead of one.
func (l *L4) handle(client net.Conn) {
	defer client.Close()

	src := client.RemoteAddr().String()
	if host, _, err := net.SplitHostPort(src); err == nil {
		src = host
	}
	tab := l.Pool.Table()
	svc := tab.Service(l.Key)
	if svc == nil {
		return
	}
	healthy := svc.Healthy()
	if l.mode() == PrimaryOnly {
		healthy = nil
		if b, ok := svc.Primary(); ok {
			healthy = []Backend{b}
		}
	}
	if len(healthy) == 0 {
		return
	}

	var backend net.Conn
	var chosen Backend
	for attempt := 0; attempt < 3; attempt++ {
		idx := l.pickAt(healthy, src)
		if idx < 0 {
			break
		}
		b := healthy[idx]
		target := svc.TargetPort
		if l.TargetPort != 0 {
			target = l.TargetPort
		}
		addr := l.Resolve(b, target)
		c, err := l.dialer(b).Dial("tcp", addr)
		if err == nil {
			backend, chosen = c, b
			break
		}
		if attempt < 2 {
			time.Sleep(dialRetryBackoff)
		}
	}
	if backend == nil {
		return
	}
	defer backend.Close()

	id := l.backendID(chosen)
	cnt := l.incActive(id)
	defer cnt.Add(-1)
	l.registerConn(id, client)
	defer l.unregisterConn(id, client)

	done := make(chan struct{}, 2)
	go func() {
		_ = copyStream(backend, client)
		// Client is done sending: half-close toward the backend so a
		// pipelined peer sees EOF without tearing the reverse path.
		if tc, ok := backend.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		_ = copyStream(client, backend)
		if tc, ok := client.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}()
	<-done
	<-done
}

// pickAt applies the configured algorithm to a candidate list.
// dialer is the backend dialer for b, bound to DialFrom's address when set.
func (l *L4) dialer(b Backend) *net.Dialer {
	d := &net.Dialer{Timeout: l.dialTimeout()}
	if l.DialFrom != nil {
		if from := l.DialFrom(b); from.IsValid() {
			d.LocalAddr = &net.TCPAddr{IP: from.AsSlice()}
		}
	}
	return d
}

func (l *L4) pickAt(cands []Backend, srcIP string) int {
	if len(cands) == 0 {
		return -1
	}
	switch l.mode() {
	case LeastConn:
		best, bestN := 0, l.activeCount(l.backendID(cands[0]))
		for i := 1; i < len(cands); i++ {
			if n := l.activeCount(l.backendID(cands[i])); n < bestN {
				best, bestN = i, n
			}
		}
		if l.MaxPerBackend > 0 && bestN >= int64(l.MaxPerBackend) {
			return -1
		}
		return best
	case SourceHash:
		h := fnv.New32a()
		_, _ = h.Write([]byte(srcIP))
		i := int(h.Sum32() % uint32(len(cands)))
		if l.MaxPerBackend > 0 && l.activeCount(l.backendID(cands[i])) >= int64(l.MaxPerBackend) {
			return -1
		}
		return i
	default: // round-robin (skips over-limit backends)
		start := int(l.rr.Add(1)-1) % len(cands)
		for i := 0; i < len(cands); i++ {
			j := (start + i) % len(cands)
			if l.MaxPerBackend > 0 && l.activeCount(l.backendID(cands[j])) >= int64(l.MaxPerBackend) {
				continue
			}
			return j
		}
		return -1
	}
}

// bufPool backs the non-splice copy path (§4.3: zero-alloc hot path —
// splice(2) is used automatically for TCP↔TCP pairs via
// TCPConn.ReadFrom; the pooled-buffer path covers anything else).
var bufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 32*1024)
		return &b
	},
}

func copyStream(dst, src net.Conn) (err error) {
	td, okD := dst.(*net.TCPConn)
	ts, okS := src.(*net.TCPConn)
	if okD && okS {
		_, err = io.Copy(td, ts) // splice when the kernel supports it
		return err
	}
	bufp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bufp)
	_, err = io.CopyBuffer(dst, src, *bufp)
	return err
}
