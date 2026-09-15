package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTable is an atomic Table source (mimics Pool's swap discipline).
type fakeTable struct {
	p atomic.Pointer[Table]
}

func (f *fakeTable) Table() *Table { return f.p.Load() }
func (f *fakeTable) set(t *Table)  { f.p.Store(t) }
func (f *fakeTable) setOne(backends ...Backend) {
	svc := &Service{Key: "default/web", Namespace: "default", Name: "web", Port: 80, TargetPort: 8080, Backends: backends}
	for i := range svc.Backends {
		svc.Backends[i].Healthy = true
	}
	f.set(&Table{Services: map[string]*Service{"default/web": svc}})
}

// fakeBackend is a tiny echo server standing in for a replica.
type fakeBackend struct {
	ln net.Listener
	id string
}

func newFakeBackend(t *testing.T, id string) *fakeBackend {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b := &fakeBackend{ln: ln, id: id}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				if _, err := fmt.Fprintf(c, "backend-%s", id); err != nil {
					return
				}
				io.Copy(c, c) // echo anything the client sends afterwards
			}(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return b
}

func backendFor(b *fakeBackend, idx int32) Backend {
	return Backend{ReplicaIndex: idx, NodeID: b.id, Healthy: true}
}

// startProxy runs an L4 over tbl and returns its local address.
func startProxy(t *testing.T, tbl *fakeTable, backends map[string]*fakeBackend, mode BalancerMode, maxPer int, drain time.Duration) net.Addr {
	t.Helper()
	l4 := &L4{
		Pool: tbl, Key: "default/web", Mode: mode,
		MaxPerBackend: maxPer, DrainTimeout: drain, DialTimeout: 2 * time.Second,
		Resolve: func(b Backend, target int32) string {
			if fb, ok := backends[b.NodeID]; ok {
				return fb.ln.Addr().String()
			}
			return ""
		},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = l4.Serve(ctx, ln) }()
	return ln.Addr()
}

func dialProxy(t *testing.T, addr string) (*bufio.Reader, net.Conn) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return bufio.NewReader(c), c
}

func readReply(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	out := make([]byte, 64)
	n, err := r.Read(out)
	if n == 0 {
		t.Fatalf("no reply: %v", err)
	}
	return string(out[:n])
}

func TestRoundRobinDistribution(t *testing.T) {
	tbl := &fakeTable{}
	b1 := newFakeBackend(t, "n1")
	b2 := newFakeBackend(t, "n2")
	b3 := newFakeBackend(t, "n3")
	backends := map[string]*fakeBackend{"n1": b1, "n2": b2, "n3": b3}
	tbl.setOne(backendFor(b1, 0), backendFor(b2, 1), backendFor(b3, 2))
	addr := startProxy(t, tbl, backends, RoundRobin, 0, DefaultDrainTimeout)

	// Deterministic given a fixed backend set + request sequence: three
	// consecutive requests hit three different backends in order.
	seen := map[string]bool{}
	want := []string{"backend-n1", "backend-n2", "backend-n3"}
	for i, w := range want {
		r, _ := dialProxy(t, addr.String())
		if got := readReply(t, r); got != w {
			t.Fatalf("request %d: got %q, want %q", i, got, w)
		}
		seen[w] = true
	}
	if len(seen) != 3 {
		t.Fatalf("round-robin did not cycle all backends: %v", seen)
	}
}

func TestLeastConnSelection(t *testing.T) {
	l4 := &L4{Mode: LeastConn}
	b1 := Backend{ReplicaIndex: 0, NodeID: "n1", Healthy: true}
	b2 := Backend{ReplicaIndex: 1, NodeID: "n2", Healthy: true}
	cands := []Backend{b1, b2}
	// No active connections → first in index order.
	if got := l4.pickAt(cands, "10.0.0.1"); got != 0 {
		t.Fatalf("empty: picked %d, want 0", got)
	}
	// One active connection on n1 → n2 wins.
	cnt := l4.incActive(l4.backendID(b1))
	cnt.Add(1)
	if got := l4.pickAt(cands, "10.0.0.1"); got != 1 {
		t.Fatalf("busy n1: picked %d, want 1", got)
	}
	// Both busy equally → index order.
	cnt2 := l4.incActive(l4.backendID(b2))
	cnt2.Add(1)
	if got := l4.pickAt(cands, "10.0.0.1"); got != 0 {
		t.Fatalf("tie: picked %d, want 0", got)
	}
}

func TestSourceHashSticky(t *testing.T) {
	l4 := &L4{Mode: SourceHash}
	b1 := Backend{ReplicaIndex: 0, NodeID: "n1", Healthy: true}
	b2 := Backend{ReplicaIndex: 1, NodeID: "n2", Healthy: true}
	b3 := Backend{ReplicaIndex: 2, NodeID: "n3", Healthy: true}
	cands := []Backend{b1, b2, b3}
	// Same source IP → same backend every time (sticky).
	first := l4.pickAt(cands, "192.168.1.9")
	for i := 0; i < 10; i++ {
		if got := l4.pickAt(cands, "192.168.1.9"); got != first {
			t.Fatalf("not sticky: pick %d gave %d, want %d", i, got, first)
		}
	}
	// Different sources distribute across the set.
	dist := map[int]bool{}
	for i := 0; i < 50; i++ {
		dist[l4.pickAt(cands, fmt.Sprintf("192.168.1.%d", 10+i))] = true
	}
	if len(dist) < 2 {
		t.Fatalf("source-hash collapsed to one backend: %v", dist)
	}
}

func TestConnectionLimit(t *testing.T) {
	l4 := &L4{}
	b1 := Backend{ReplicaIndex: 0, NodeID: "n1", Healthy: true}
	b2 := Backend{ReplicaIndex: 1, NodeID: "n2", Healthy: true}
	cands := []Backend{b1, b2}
	// n1 at its limit → the scan skips it and picks n2.
	for i := 0; i < 3; i++ {
		c := l4.incActive(l4.backendID(b1))
		c.Add(1)
	}
	l4.MaxPerBackend = 3
	if got := l4.pickAt(cands, "10.0.0.1"); got != 1 {
		t.Fatalf("over-limit n1 still picked: %d", got)
	}
	// Every backend at limit → refuse.
	for i := 0; i < 3; i++ {
		c := l4.incActive(l4.backendID(b2))
		c.Add(1)
	}
	if got := l4.pickAt(cands, "10.0.0.1"); got != -1 {
		t.Fatalf("all-full should refuse, picked %d", got)
	}
}

func TestDrainRemovedBackend(t *testing.T) {
	tbl := &fakeTable{}
	b1 := newFakeBackend(t, "n1")
	b2 := newFakeBackend(t, "n2")
	backends := map[string]*fakeBackend{"n1": b1, "n2": b2}
	tbl.setOne(backendFor(b1, 0), backendFor(b2, 1))
	addr := startProxy(t, tbl, backends, RoundRobin, 0, 400*time.Millisecond)

	// Open one connection (round-robin → n1) and verify it works.
	r1, c1 := dialProxy(t, addr.String())
	reply := readReply(t, r1)
	live := map[string]bool{"backend-n1": true, "backend-n2": true}
	if !live[reply] {
		t.Fatalf("unexpected first reply %q", reply)
	}
	liveBackend := map[string]*fakeBackend{"backend-n1": b1, "backend-n2": b2}[reply]

	// Remove the backend mid-connection.
	if liveBackend == b1 {
		tbl.setOne(backendFor(b2, 1))
	} else {
		tbl.setOne(backendFor(b1, 0))
	}

	// Existing connection still passes data (drain, not kill): the echo
	// channel keeps working after the removal.
	if _, err := c1.Write([]byte("ping")); err != nil {
		t.Fatalf("drained conn could not send: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	buf := make([]byte, 4)
	for {
		n, err := c1.Read(buf)
		if n > 0 {
			break
		}
		if err != nil || time.Now().After(deadline) {
			t.Fatalf("drained conn died immediately: %v", err)
		}
	}

	// ...but never longer than DrainTimeout: the force-close lands
	// within ~1 s of the 400 ms limit.
	time.Sleep(1200 * time.Millisecond)
	c1.SetReadDeadline(time.Now().Add(1 * time.Second))
	if _, err := c1.Read(buf); err == nil {
		t.Fatalf("connection survived past DrainTimeout")
	}
}

func TestDrainStopsNewWorkImmediately(t *testing.T) {
	tbl := &fakeTable{}
	b1 := newFakeBackend(t, "n1")
	backends := map[string]*fakeBackend{"n1": b1}
	tbl.setOne(backendFor(b1, 0))
	addr := startProxy(t, tbl, backends, RoundRobin, 0, DefaultDrainTimeout)

	// Backend removed from the table: new connections get no backends
	// and are closed immediately (no reply, prompt EOF).
	tbl.setOne()
	for i := 0; i < 3; i++ {
		r, c := dialProxy(t, addr.String())
		c.SetReadDeadline(time.Now().Add(1 * time.Second))
		if n, _ := r.Read(make([]byte, 32)); n != 0 {
			t.Fatalf("request %d reached a removed backend", i)
		}
	}
}

// compile-time check that *Pool satisfies TableSource.
var _ TableSource = (*Pool)(nil)
