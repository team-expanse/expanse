package raftstore_test

import (
	"testing"
	"time"

	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/raftstore"
)

// TestLinearizableReadsAfterAck (§4.1): a write acked on any node is
// visible to a linearizable read on every node.
func TestLinearizableReadsAfterAck(t *testing.T) {
	c := NewTestCluster(t, 3)
	f := c.Follower()
	if _, err := f.Put(t.Context(), "/lin", []byte("v1")); err != nil {
		t.Fatalf("put via follower: %v", err)
	}
	for i, n := range c.Nodes {
		e, err := n.Get(t.Context(), "/lin")
		if err != nil {
			t.Fatalf("node %d get: %v", i, err)
		}
		if string(e.Value) != "v1" {
			t.Errorf("node %d value = %q, want v1", i, e.Value)
		}
	}
}

// TestForwardViaFollower: writes sent to a follower are forwarded to the
// leader, applied through Raft, and the committed revision is returned to
// the caller that started on the follower.
func TestForwardViaFollower(t *testing.T) {
	c := NewTestCluster(t, 3)
	f := c.Follower()
	rev, err := f.Put(t.Context(), "/fwd", []byte("x"))
	if err != nil {
		t.Fatalf("put via follower: %v", err)
	}
	if rev == 0 {
		t.Fatal("forwarded write returned revision 0")
	}
	// Typed errors survive the hop: a conflicting CAS forwarded to the
	// leader must come back as KindConflict, not a transport error.
	if _, err := f.CompareAndSwap(t.Context(), "/fwd", 99999, []byte("y")); !errors.Is(err, errors.KindConflict) {
		t.Errorf("forwarded CAS failure: got %v, want KindConflict", err)
	}
}

// TestNoLeaderReadOnly: a node with no leader (never bootstrapped) fails
// linearizable reads and writes with KindUnavailable after the leader-wait
// deadline (§4.10 degraded read-only mode), while explicit stale reads
// still work off local state.
func TestNoLeaderReadOnly(t *testing.T) {
	old := raftstore.LeaderWaitTimeout
	raftstore.LeaderWaitTimeout = 300 * time.Millisecond
	t.Cleanup(func() { raftstore.LeaderWaitTimeout = old })

	s, err := raftstore.Open(raftstore.Config{
		NodeID:   "lonely",
		BindAddr: freeAddr(t),
		DataDir:  t.TempDir(),
		// Bootstrap: false — never forms a cluster, never elects a leader.
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.Get(t.Context(), "/x"); !errors.Is(err, errors.KindUnavailable) {
		t.Errorf("linearizable get without leader: got %v, want KindUnavailable", err)
	}
	if _, err := s.Put(t.Context(), "/x", []byte("v")); !errors.Is(err, errors.KindUnavailable) {
		t.Errorf("write without leader: got %v, want KindUnavailable", err)
	}
	if err := s.Delete(t.Context(), "/x", 0); !errors.Is(err, errors.KindUnavailable) {
		t.Errorf("delete without leader: got %v, want KindUnavailable", err)
	}
}

// TestStaleReadsServeDuringPartition (§4.10, §10 "Followers serving stale
// reads"): with quorum gone the cluster is read-only — linearizable reads
// fail with KindUnavailable, but WithStale reads keep serving committed
// local state.
func TestStaleReadsServeDuringPartition(t *testing.T) {
	old := raftstore.LeaderWaitTimeout
	raftstore.LeaderWaitTimeout = 500 * time.Millisecond
	t.Cleanup(func() { raftstore.LeaderWaitTimeout = old })

	c := NewTestCluster(t, 3)
	leader := c.Leader()
	if _, err := leader.Put(t.Context(), "/committed", []byte("yes")); err != nil {
		t.Fatal(err)
	}
	// Leave exactly one FORMER FOLLOWER alive: a lone follower has no
	// quorum (the dead leader keeps its voter slot) and cannot win an
	// election, so it must degrade to read-only. (A leader+follower pair
	// would retain quorum and keep serving linearizable reads.)
	victim := c.Follower()
	// Wait until the victim's own FSM has the entry (majority commit may
	// not include the victim) before cutting the cluster down.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := victim.Get(store.WithStale(t.Context()), "/committed"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("victim never replicated /committed")
		}
		time.Sleep(20 * time.Millisecond)
	}
	for i := range c.Nodes {
		if c.Nodes[i] != victim {
			c.Kill(i)
		}
	}

	// Linearizable read on the survivor fails (no quorum → no barrier).
	if _, err := victim.Get(t.Context(), "/committed"); !errors.Is(err, errors.KindUnavailable) {
		t.Errorf("linearizable read without quorum: got %v, want KindUnavailable", err)
	}
	// Stale read serves the committed state.
	e, err := victim.Get(store.WithStale(t.Context()), "/committed")
	if err != nil {
		t.Fatalf("stale read without quorum: %v", err)
	}
	if string(e.Value) != "yes" {
		t.Errorf("stale read value = %q, want yes", e.Value)
	}
}

// TestStaleReadMustNotSeeUncommitted: a stale read may lag but never
// fabricates — it returns only entries that some leader committed
// (here: it cannot observe a key written after the local FSM stopped
// tracking; the practical assertion is stale read never returns a
// HIGHER revision than the cluster's committed one).
func TestStaleReadRevisionMonotone(t *testing.T) {
	c := NewTestCluster(t, 3)
	f := c.Follower()
	r1, err := f.Revision(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := c.Leader().Put(t.Context(), "/m", []byte("v")); err != nil {
			t.Fatal(err)
		}
	}
	r2, err := f.Revision(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	rs, err := f.Revision(store.WithStale(t.Context()))
	if err != nil {
		t.Fatal(err)
	}
	if r2 < r1 {
		t.Errorf("linearizable revision went backwards: %d < %d", r2, r1)
	}
	if rs > r2 {
		t.Errorf("stale revision %d exceeds linearizable revision %d", rs, r2)
	}
}
