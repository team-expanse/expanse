package membership_test

import (
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/cluster/membership"
)

// freePort grabs an ephemeral TCP port (memberlist binds TCP+UDP on the
// same port; grabbing TCP first makes a UDP collision vanishingly rare).
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("grab port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

type agent struct {
	a      *membership.Agent
	events chan membership.Event
}

// newAgent starts one memberlist agent on a loopback port.
func newAgent(t *testing.T, id string, port int, secret []byte, opts ...func(*membership.Config)) agent {
	t.Helper()
	conf := membership.Config{
		NodeID:        id,
		BindAddr:      "127.0.0.1",
		BindPort:      port,
		ClusterSecret: secret,
		Meta: membership.NodeMeta{
			NodeID:   id,
			Role:     membership.RoleVoter,
			Version:  "0.1.0",
			RaftAddr: fmt.Sprintf("127.0.0.1:%d", port+1),
			APIAddr:  fmt.Sprintf("127.0.0.1:%d", port+2),
		},
	}
	ev := make(chan membership.Event, 64)
	conf.OnEvent = func(e membership.Event) {
		select {
		case ev <- e:
		default: // never block memberlist's handoff goroutine
		}
	}
	for _, o := range opts {
		o(&conf)
	}
	a, err := membership.NewAgent(conf)
	if err != nil {
		t.Fatalf("NewAgent %s: %v", id, err)
	}
	t.Cleanup(func() {
		_ = a.Shutdown()
	})
	return agent{a: a, events: ev}
}

var (
	secretA = []byte("cluster-secret-A-0123456789abcdef")
	secretB = []byte("cluster-secret-B-0123456789abcdef")
)

// TestThreeMemberMesh: a 3-node mesh forms, every node sees every other
// node, and gossiped metadata round-trips.
func TestThreeMemberMesh(t *testing.T) {
	ports := []int{freePort(t), freePort(t), freePort(t)}
	nodes := []agent{
		newAgent(t, "n0", ports[0], secretA),
		newAgent(t, "n1", ports[1], secretA),
		newAgent(t, "n2", ports[2], secretA),
	}

	// n1 and n2 join n0.
	for i := 1; i < 3; i++ {
		n, err := nodes[i].a.Join([]string{fmt.Sprintf("127.0.0.1:%d", ports[0])})
		if err != nil || n == 0 {
			t.Fatalf("node %d join: %d nodes contacted, err=%v", i, n, err)
		}
	}

	// Every node must see all three with alive state and parsed metadata.
	deadline := time.Now().Add(5 * time.Second)
	for {
		ok := true
		for i, nd := range nodes {
			got := nd.a.Members()
			if len(got) != 3 {
				ok = false
				break
			}
			byID := map[string]membership.NodeState{}
			for _, s := range got {
				byID[s.Meta.NodeID] = s
			}
			for j := 0; j < 3; j++ {
				s, present := byID[fmt.Sprintf("n%d", j)]
				if !present || s.State != "alive" {
					ok = false
					break
				}
				if s.Meta.RaftAddr == "" || s.Meta.APIAddr == "" {
					ok = false // metadata did not round-trip
					break
				}
			}
			_ = i
		}
		if ok {
			return
		}
		if time.Now().After(deadline) {
			for i, nd := range nodes {
				t.Errorf("node %d members: %+v", i, nd.a.Members())
			}
			t.Fatal("mesh did not converge within 5s")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestDeadNodeDetection: a node that crashes (Shutdown without Leave — the
// semantics of a power loss) is suspected and confirmed dead by survivors
// in ~3s (SuspicionMult 3 × ProbeInterval 1s × log(N+1)); the survivors
// receive an EventLeave.
func TestDeadNodeDetection(t *testing.T) {
	ports := []int{freePort(t), freePort(t), freePort(t)}
	nodes := []agent{
		newAgent(t, "n0", ports[0], secretA),
		newAgent(t, "n1", ports[1], secretA),
		newAgent(t, "n2", ports[2], secretA),
	}
	for i := 1; i < 3; i++ {
		if _, err := nodes[i].a.Join([]string{fmt.Sprintf("127.0.0.1:%d", ports[0])}); err != nil {
			t.Fatalf("node %d join: %v", i, err)
		}
	}
	waitMesh(t, nodes, 3, 5*time.Second)

	// Crash n2 (no graceful leave).
	crashedAt := time.Now()
	if err := nodes[2].a.Shutdown(); err != nil {
		t.Fatalf("victim shutdown: %v", err)
	}

	// Survivors must mark it dead via an EventLeave. Allow generous wall
	// clock for CI but assert it did not take absurdly long either.
	// memberlist has a GossipToTheDeadTime for re-broadcasts, but
	// first detection is bound by suspicion: ~3*log(N+1)*ProbeInterval
	// ≈ 3s at N=3 (a grace factor for indirect probes applies).
	evs := make([]membership.Event, 0, 2)
	var mu sync.Mutex
	gotLeave := make(chan struct{})
	go func() {
		for e := range nodes[0].events {
			mu.Lock()
			evs = append(evs, e)
			mu.Unlock()
			if e.Kind == membership.EventLeave && e.Node.NodeID == "n2" {
				close(gotLeave)
				return
			}
		}
	}()
	select {
	case <-gotLeave:
	case <-time.After(15 * time.Second):
		mu.Lock()
		t.Fatalf("no leave event for n2 within 15s; events=%v", evs)
	}
	d := time.Since(crashedAt)
	if d < 500*time.Millisecond {
		// detection cannot be faster than one full probe cycle +
		// suspicion window; <0.5s means we observed a graceful path
		t.Errorf("leave observed in %v — too fast for a crash", d)
	}
	t.Logf("dead-node detection took %v", d)

	// The recorded state is dead and read-only liveness: nothing here
	// touched any (future) Raft membership.
	if s, ok := nodes[0].a.State("n2"); ok && s.State != "dead" && s.State != "left" {
		t.Errorf("n2 state = %q, want dead", s.State)
	}
}

// TestKeyMismatchNoJoin: agents with different cluster secrets derive
// different gossip keys; encrypted messages are rejected, so the meshes
// must not merge.
func TestKeyMismatchNoJoin(t *testing.T) {
	port0 := freePort(t)
	a := newAgent(t, "good", port0, secretA)
	b := newAgent(t, "bad", freePort(t), secretB)

	_, err := b.a.Join([]string{fmt.Sprintf("127.0.0.1:%d", port0)})
	if err == nil {
		// memberlist sometimes completes the TCP join but rejects the
		// encrypted sync; verify from membership instead.
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if len(a.a.Members()) > 0 || len(b.a.Members()) > 0 {
				t.Fatalf("key-mismatched agents merged: %v / %v",
					a.a.Members(), b.a.Members())
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Log("join succeeded but meshes stayed separate (encrypted sync rejected)")
	}
}

// TestMetaUpdate: UpdateMeta re-gossips changed metadata; peers observe
// the new role.
func TestMetaUpdate(t *testing.T) {
	port0, port1 := freePort(t), freePort(t)
	a := newAgent(t, "n0", port0, secretA)
	b := newAgent(t, "n1", port1, secretA)
	if _, err := b.a.Join([]string{fmt.Sprintf("127.0.0.1:%d", port0)}); err != nil {
		t.Fatalf("join: %v", err)
	}
	waitMesh(t, []agent{a, b}, 2, 5*time.Second)

	if err := a.a.UpdateMeta(membership.NodeMeta{
		NodeID: "n0", Role: membership.RoleWitness, Version: "0.1.0",
		RaftAddr: fmt.Sprintf("127.0.0.1:%d", port0+1),
		APIAddr:  fmt.Sprintf("127.0.0.1:%d", port0+2),
	}); err != nil {
		t.Fatalf("UpdateMeta: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if s, ok := b.a.State("n0"); ok && s.Meta.Role == membership.RoleWitness {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("role change not observed: %+v", mustState(t, b, "n0"))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func waitMesh(t *testing.T, nodes []agent, n int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		ok := true
		for _, nd := range nodes {
			if len(nd.a.Members()) != n {
				ok = false
			}
		}
		if ok {
			return
		}
		if time.Now().After(deadline) {
			for i, nd := range nodes {
				t.Errorf("node %d members: %+v", i, nd.a.Members())
			}
			t.Fatalf("mesh did not converge to %d members", n)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func mustState(t *testing.T, a agent, id string) membership.NodeState {
	t.Helper()
	s, ok := a.a.State(id)
	if !ok {
		t.Fatalf("no state for %s", id)
	}
	return s
}
