package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
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

// setOnePrimary is setOne plus a PrimaryNodeID, for PrimaryOnly mode
// tests. Also sets ConfirmedPrimaryNodeID to match: these tests exercise
// L4's OWN routing behavior given an already-decided Primary(), not the
// confirmation gate itself (TestPoolPrimaryRequiresConfirmationToMatchTheCurrentLeaseHolder
// covers that, in pool_test.go). An empty primary leaves both empty too,
// matching Primary()'s own "no election decided yet" case.
func (f *fakeTable) setOnePrimary(primary string, backends ...Backend) {
	svc := &Service{
		Key: "default/web", Namespace: "default", Name: "web", Port: 80, TargetPort: 8080,
		Backends: backends, PrimaryNodeID: primary, ConfirmedPrimaryNodeID: primary,
	}
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

// TestPrimaryOnlyRoutesOnlyToElectedBackend is D2: every request must
// land on the backend the election lease names, never round-robin
// across the block's other, non-interchangeable replicas.
func TestPrimaryOnlyRoutesOnlyToElectedBackend(t *testing.T) {
	tbl := &fakeTable{}
	b1 := newFakeBackend(t, "n1")
	b2 := newFakeBackend(t, "n2")
	backends := map[string]*fakeBackend{"n1": b1, "n2": b2}
	tbl.setOnePrimary("n2", backendFor(b1, 0), backendFor(b2, 1))
	addr := startProxy(t, tbl, backends, PrimaryOnly, 0, DefaultDrainTimeout)

	for i := 0; i < 5; i++ {
		r, _ := dialProxy(t, addr.String())
		if got := readReply(t, r); got != "backend-n2" {
			t.Fatalf("request %d: got %q, want backend-n2 (the elected primary)", i, got)
		}
	}
}

// TestPrimaryOnlyRefusesBeforeElection: no election decided yet (D2's
// "do not guess" rule) must refuse the connection, not fall back to
// round-robin across an arbitrary replica.
func TestPrimaryOnlyRefusesBeforeElection(t *testing.T) {
	tbl := &fakeTable{}
	b1 := newFakeBackend(t, "n1")
	backends := map[string]*fakeBackend{"n1": b1}
	tbl.setOnePrimary("", backendFor(b1, 0))
	addr := startProxy(t, tbl, backends, PrimaryOnly, 0, DefaultDrainTimeout)

	r, c := dialProxy(t, addr.String())
	_ = r
	c.SetReadDeadline(time.Now().Add(1 * time.Second))
	if n, _ := c.Read(make([]byte, 32)); n != 0 {
		t.Fatalf("request reached a backend before any election decided")
	}
}

// TestHandleRetriesDialAgainstTheSoleCandidateOnTransientFailure is the
// regression test for the Stream D vertical-slice VM test's own find:
// PrimaryOnly's candidate set is never more than one backend (D2), so a
// single transient dial failure against it must not close the client's
// connection outright with nothing left to retry -- the old
// shrink-the-candidate-list-on-failure logic did exactly that. The
// FIRST dial targets a closed port (a real, addressable TCP endpoint
// simulating "briefly not accepting," not "actually down"); every
// attempt after resolves to the real, live backend.
func TestHandleRetriesDialAgainstTheSoleCandidateOnTransientFailure(t *testing.T) {
	tbl := &fakeTable{}
	b1 := newFakeBackend(t, "n1")
	backends := map[string]*fakeBackend{"n1": b1}
	tbl.setOnePrimary("n1", backendFor(b1, 0))

	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := dead.Addr().String()
	dead.Close()

	var calls atomic.Int32
	l4 := &L4{
		Pool: tbl, Key: "default/web", Mode: PrimaryOnly, DialTimeout: 2 * time.Second,
		Resolve: func(b Backend, target int32) string {
			if calls.Add(1) == 1 {
				return deadAddr
			}
			return backends[b.NodeID].ln.Addr().String()
		},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l4.Close(); ln.Close() })
	go func() { _ = l4.Serve(context.Background(), ln) }()

	r, _ := dialProxy(t, ln.Addr().String())
	if got := readReply(t, r); got != "backend-n1" {
		t.Fatalf("got %q, want backend-n1 -- a single transient dial failure against PrimaryOnly's only candidate must not close the client's connection with nothing left to retry", got)
	}
	if got := calls.Load(); got < 2 {
		t.Fatalf("Resolve called %d times, want at least 2 (a retry after the first transient failure)", got)
	}
}

// flakyListener injects a fixed number of non-close Accept() errors
// before delegating to the real listener — a pending client connection
// simply waits in the kernel's own accept backlog across those retries,
// exactly as it would against a real, momentarily resource-exhausted
// listener.
type flakyListener struct {
	net.Listener
	failures atomic.Int32
}

func (f *flakyListener) Accept() (net.Conn, error) {
	if f.failures.Add(-1) >= 0 {
		return nil, &net.OpError{Op: "accept", Err: fmt.Errorf("simulated transient accept error")}
	}
	f.failures.Add(1) // don't let it go further negative than needed
	return f.Listener.Accept()
}

// TestServeRetriesAfterATransientAcceptError is the regression test for
// the Stream D vertical-slice VM test's own find: a transient Accept()
// error (not this listener being intentionally Close()d) must not kill
// the whole accept loop — internal/agent's own lbListen never
// re-invokes Serve short of a full VIP handover, so one hiccup used to
// mean every later connection to the VIP went nowhere for the rest of
// that holder's tenure.
func TestServeRetriesAfterATransientAcceptError(t *testing.T) {
	tbl := &fakeTable{}
	b1 := newFakeBackend(t, "n1")
	backends := map[string]*fakeBackend{"n1": b1}
	tbl.setOnePrimary("n1", backendFor(b1, 0))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fl := &flakyListener{Listener: ln}
	fl.failures.Store(3)

	l4 := &L4{
		Pool: tbl, Key: "default/web", Mode: PrimaryOnly, DialTimeout: 2 * time.Second,
		Resolve: func(b Backend, target int32) string { return backends[b.NodeID].ln.Addr().String() },
	}
	t.Cleanup(func() { l4.Close(); ln.Close() })
	done := make(chan error, 1)
	go func() { done <- l4.Serve(context.Background(), fl) }()

	r, _ := dialProxy(t, ln.Addr().String())
	if got := readReply(t, r); got != "backend-n1" {
		t.Fatalf("got %q, want backend-n1 -- transient Accept() errors must not permanently kill the listener", got)
	}
	select {
	case err := <-done:
		t.Fatalf("Serve returned early despite only transient Accept() errors: %v", err)
	default:
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

// Regression: a single healthy backend that fails to dial used to crash
// the whole agent (handle's retry loop shrank `remaining` by 2 instead
// of 1 each attempt, then indexed remaining[:-1] once it emptied — see
// l4.go's comment). Any single-replica block (SINGLETON, or plain
// replicas=1) briefly unreachable when a client connected would take
// the whole node's proxy down with it; nothing before share/smb ever
// exercised a VIP-exposed block with exactly one backend under this
// exact race. A dead listener stands in for "briefly unreachable".
func TestHandleOneUnreachableBackendDoesNotPanic(t *testing.T) {
	tbl := &fakeTable{}
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := dead.Addr().String()
	dead.Close() // nothing listens here now; dialing it fails
	tbl.setOne(Backend{ReplicaIndex: 0, NodeID: "only", Healthy: true})
	l4 := &L4{
		Pool: tbl, Key: "default/web", DialTimeout: 200 * time.Millisecond,
		Resolve: func(Backend, int32) string { return deadAddr },
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = l4.Serve(ctx, ln) }()

	c, err := net.DialTimeout("tcp", ln.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer c.Close()
	// No backend could be reached: handle must close the connection
	// cleanly, not panic (which would crash this whole test binary).
	buf := make([]byte, 1)
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if n, err := c.Read(buf); n != 0 || err != io.EOF {
		t.Fatalf("read = %d, %v; want a clean EOF", n, err)
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

func TestDialFromBindsTheBackendConnectionsSourceAddress(t *testing.T) {
	tbl := &fakeTable{}
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { backend.Close() })
	seen := make(chan string, 1)
	go func() {
		c, err := backend.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		seen <- c.RemoteAddr().(*net.TCPAddr).IP.String()
	}()
	tbl.setOne(Backend{ReplicaIndex: 0, NodeID: "n1"})
	l4 := &L4{
		Pool: tbl, Key: "default/web", DialTimeout: 2 * time.Second,
		Resolve:  func(Backend, int32) string { return backend.Addr().String() },
		DialFrom: func(Backend) netip.Addr { return netip.MustParseAddr("127.0.0.7") },
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l4.Close(); ln.Close() })
	go func() { _ = l4.Serve(context.Background(), ln) }()

	dialProxy(t, ln.Addr().String())
	select {
	case got := <-seen:
		if got != "127.0.0.7" {
			t.Fatalf("backend saw source %s, want 127.0.0.7", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("proxy never dialed the backend")
	}
}

func TestL4TargetPortOverridesTheServicePort(t *testing.T) {
	tbl := &fakeTable{}
	b1 := newFakeBackend(t, "n1")
	tbl.setOne(backendFor(b1, 0))
	var asked atomic.Int32
	l4 := &L4{
		Pool: tbl, Key: "default/web", TargetPort: 8443, DialTimeout: 2 * time.Second,
		Resolve: func(b Backend, target int32) string {
			asked.Store(target)
			return b1.ln.Addr().String()
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
	r, _ := dialProxy(t, ln.Addr().String())
	if got := readReply(t, r); got != "backend-n1" {
		t.Fatalf("reply %q", got)
	}
	// A block's second VIP port forwards to its own target, not the first port's 8080.
	if got := asked.Load(); got != 8443 {
		t.Errorf("resolved target port %d, want 8443", got)
	}
}
