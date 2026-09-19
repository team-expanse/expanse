package exvol

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
)

// Faults is the opt-in network/control-plane fault layer (T21). Every
// ordered node pair (src→dst) gets its own loopback proxy, so a fault can
// hit exactly one direction of one link; and each node's view of the
// shared store (the raft-log stand-in) is gated, so an isolated node also
// loses lease renewal — the way a real minority partition does.
//
//	Isolate  partition: links severed, store unreachable (errors)
//	Pause    SIGSTOP analog: links and store calls stall, nothing errors
//	SetDelay slow node: every byte through its links is delayed
type Faults struct {
	mu       sync.Mutex
	cond     *sync.Cond
	stopped  bool
	isolated map[int]bool
	paused   map[int]bool
	delay    map[int]time.Duration
	links    []*link
}

type link struct {
	src, dst int
	ln       net.Listener
	mu       sync.Mutex
	conns    map[net.Conn]struct{}
}

func newFaults() *Faults {
	f := &Faults{isolated: map[int]bool{}, paused: map[int]bool{}, delay: map[int]time.Duration{}}
	f.cond = sync.NewCond(&f.mu)
	return f
}

func linkIP(src, dst int) string { return fmt.Sprintf("127.1.%d.%d", src, dst) }
func backIP(dst int) string      { return fmt.Sprintf("127.0.100.%d", dst) }

// addLink starts the src→dst proxy on the shared replication port.
func (f *Faults) addLink(src, dst, port int) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", linkIP(src, dst), port))
	if err != nil {
		return err
	}
	l := &link{src: src, dst: dst, ln: ln, conns: map[net.Conn]struct{}{}}
	f.links = append(f.links, l)
	go f.accept(l, fmt.Sprintf("%s:%d", backIP(dst), port))
	return nil
}

func (f *Faults) accept(l *link, backend string) {
	for {
		c, err := l.ln.Accept()
		if err != nil {
			return
		}
		go f.serve(l, c, backend)
	}
}

func (f *Faults) serve(l *link, c net.Conn, backend string) {
	if !f.reachable(l.src, l.dst) {
		_ = c.Close()
		return
	}
	b, err := net.DialTimeout("tcp", backend, time.Second)
	if err != nil {
		_ = c.Close()
		return
	}
	l.track(c, b)
	go f.pipe(l, c, b)
	f.pipe(l, b, c)
}

// pipe copies a→b one chunk at a time, applying pause/partition/delay.
func (f *Faults) pipe(l *link, a, b net.Conn) {
	defer func() { _ = a.Close(); _ = b.Close(); l.untrack(a, b) }()
	buf := make([]byte, 64<<10)
	for {
		n, err := a.Read(buf)
		if n > 0 {
			if !f.gate(l.src, l.dst) {
				return
			}
			if _, werr := b.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// gate blocks while either end is paused, then applies the link delay;
// false means the link is partitioned (or the layer stopped).
func (f *Faults) gate(src, dst int) bool {
	f.mu.Lock()
	for (f.paused[src] || f.paused[dst]) && !f.stopped {
		f.cond.Wait()
	}
	ok := !f.stopped && !f.isolated[src] && !f.isolated[dst]
	d := f.delay[src] + f.delay[dst]
	f.mu.Unlock()
	if d > 0 {
		time.Sleep(d)
	}
	return ok
}

func (f *Faults) reachable(src, dst int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.stopped && !f.isolated[src] && !f.isolated[dst]
}

func (l *link) track(cs ...net.Conn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range cs {
		l.conns[c] = struct{}{}
	}
}

func (l *link) untrack(cs ...net.Conn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range cs {
		delete(l.conns, c)
	}
}

func (l *link) sever() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for c := range l.conns {
		_ = c.Close()
	}
}

// Isolate partitions node idx from every peer and from the control plane.
func (f *Faults) Isolate(idx int) {
	f.mu.Lock()
	f.isolated[idx] = true
	links := append([]*link(nil), f.links...)
	f.cond.Broadcast()
	f.mu.Unlock()
	for _, l := range links {
		if l.src == idx || l.dst == idx {
			l.sever()
		}
	}
}

// Pause stalls node idx (links + store calls) without erroring anything.
func (f *Faults) Pause(idx int) { f.set(f.paused, idx, true) }

// Resume undoes Pause.
func (f *Faults) Resume(idx int) { f.set(f.paused, idx, false) }

// Heal undoes Isolate.
func (f *Faults) Heal(idx int) { f.set(f.isolated, idx, false) }

func (f *Faults) set(m map[int]bool, idx int, v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m[idx] = v
	f.cond.Broadcast()
}

// SetDelay adds d of latency to every chunk on node idx's links (0 clears).
func (f *Faults) SetDelay(idx int, d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delay[idx] = d
}

// HealAll clears every fault.
func (f *Faults) HealAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.isolated, f.paused, f.delay = map[int]bool{}, map[int]bool{}, map[int]time.Duration{}
	f.cond.Broadcast()
}

// Paused reports whether node idx is currently paused.
func (f *Faults) Paused(idx int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paused[idx]
}

// WaitRunning blocks a caller "inside" node idx while it is paused (a
// SIGSTOPped process serves no request until SIGCONT).
func (f *Faults) WaitRunning(idx int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for f.paused[idx] && !f.stopped {
		f.cond.Wait()
	}
}

// stop tears every proxy down and releases all waiters.
func (f *Faults) stop() {
	f.mu.Lock()
	f.stopped = true
	links := append([]*link(nil), f.links...)
	f.cond.Broadcast()
	f.mu.Unlock()
	for _, l := range links {
		_ = l.ln.Close()
		l.sever()
	}
}

// storeGate returns nil to let a store call through: it stalls while idx
// is paused and fails while idx is isolated.
func (f *Faults) storeGate(idx int) error {
	f.WaitRunning(idx)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.isolated[idx] {
		return experrors.New(experrors.KindUnavailable, "chaos.partition", fmt.Sprintf("node %d cannot reach the control plane", idx))
	}
	return nil
}

// gatedStore is one node's fault-controlled view of the shared store.
type gatedStore struct {
	store.Store
	f   *Faults
	idx int
}

func (g *gatedStore) Get(ctx context.Context, k store.Key) (*store.Entry, error) {
	if err := g.f.storeGate(g.idx); err != nil {
		return nil, err
	}
	return g.Store.Get(ctx, k)
}

func (g *gatedStore) List(ctx context.Context, p store.Key) ([]*store.Entry, error) {
	if err := g.f.storeGate(g.idx); err != nil {
		return nil, err
	}
	return g.Store.List(ctx, p)
}

func (g *gatedStore) Put(ctx context.Context, k store.Key, v []byte) (store.Revision, error) {
	if err := g.f.storeGate(g.idx); err != nil {
		return 0, err
	}
	return g.Store.Put(ctx, k, v)
}

func (g *gatedStore) CompareAndSwap(ctx context.Context, k store.Key, e store.Revision, v []byte) (store.Revision, error) {
	if err := g.f.storeGate(g.idx); err != nil {
		return 0, err
	}
	return g.Store.CompareAndSwap(ctx, k, e, v)
}

func (g *gatedStore) Delete(ctx context.Context, k store.Key, e store.Revision) error {
	if err := g.f.storeGate(g.idx); err != nil {
		return err
	}
	return g.Store.Delete(ctx, k, e)
}

func (g *gatedStore) Txn(ctx context.Context, ops []store.Op) (store.Revision, error) {
	if err := g.f.storeGate(g.idx); err != nil {
		return 0, err
	}
	return g.Store.Txn(ctx, ops)
}

func (g *gatedStore) Watch(ctx context.Context, p store.Key, from store.Revision) (<-chan store.Event, error) {
	if err := g.f.storeGate(g.idx); err != nil {
		return nil, err
	}
	return g.Store.Watch(ctx, p, from)
}

func (g *gatedStore) Revision(ctx context.Context) (store.Revision, error) {
	if err := g.f.storeGate(g.idx); err != nil {
		return 0, err
	}
	return g.Store.Revision(ctx)
}
