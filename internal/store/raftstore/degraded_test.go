package raftstore_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/raftstore"
)

// degradedRig is a 2-node cluster with a short DegradedAfter so the
// §4.10.3 window is testable. Node 0 bootstraps; node 1 joins as voter.
// Node 0's dir/port are returned so tests can "restart" it — a real
// rejoin from persistent state, which is how quorum recovers in the
// field.
func degradedRig(t *testing.T) (s0, s1 *raftstore.Store, restart0 func() *raftstore.Store) {
	t.Helper()
	d0, d1 := t.TempDir(), t.TempDir()
	p0, p1 := freePort(t), freePort(t)
	const da = 600 * time.Millisecond

	open := func(id, dir string, port int, bootstrap bool) *raftstore.Store {
		t.Helper()
		s, err := raftstore.Open(raftstore.Config{
			NodeID: id, BindAddr: fmt.Sprintf("127.0.0.1:%d", port), DataDir: dir,
			Bootstrap: bootstrap, DegradedAfter: da,
		})
		if err != nil {
			t.Fatalf("open %s: %v", id, err)
		}
		return s
	}

	s0 = open("n0", d0, p0, true)
	t.Cleanup(func() { _ = s0.Close() })
	s1 = open("n1", d1, p1, false)
	t.Cleanup(func() { _ = s1.Close() })
	waitLeaderSeenT(t, s0)
	if err := s0.AddVoter("n1", fmt.Sprintf("127.0.0.1:%d", p1)); err != nil {
		t.Fatalf("add voter: %v", err)
	}
	waitLeaderSeenT(t, s1)

	restart0 = func() *raftstore.Store {
		s0 = open("n0", d0, p0, true) // same dir: rejoin from persistent state
		t.Cleanup(func() { _ = s0.Close() })
		return s0
	}
	return s0, s1, restart0
}

// waitLeaderSeenT waits until st can SEE a leader (not necessarily be
// it) — used for joiners and followers.
func waitLeaderSeenT(t *testing.T, st *raftstore.Store) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for st.Leader() == "" {
		if time.Now().After(deadline) {
			t.Fatalf("node %s: no leader visible within timeout", st.NodeID())
		}
		time.Sleep(30 * time.Millisecond)
	}
}

func waitForDegraded(t *testing.T, st *raftstore.Store) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !st.Degraded() {
		if time.Now().After(deadline) {
			t.Fatal("node never degraded")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestDegradedNoQuorumReadOnly (§4.10.3): after the leader disappears,
// the surviving minority must go read-only within DegradedAfter (+
// election timeouts), refuse writes with KindUnavailable, and keep
// serving stale reads. DefaultDegradedAfter is 5 s (spec: "no quorum
// for > 5 s"); the rig shortens it for test speed.
func TestDegradedNoQuorumReadOnly(t *testing.T) {
	if raftstore.DefaultDegradedAfter != 5*time.Second {
		t.Fatalf("DefaultDegradedAfter = %v, want 5s (§4.10.3)", raftstore.DefaultDegradedAfter)
	}
	s0, s1, _ := degradedRig(t)
	ctx := t.Context()

	// Seed state while the cluster is healthy, then wait for the
	// follower's FSM to APPLY it (a follower acks the log append before
	// it learns the new commit index — one heartbeat later).
	if _, err := s0.Put(ctx, "/deg/k", []byte("v1")); err != nil {
		t.Fatalf("seed put: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := s1.Get(store.WithStale(ctx), "/deg/k"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("follower never applied the seed entry")
		}
		time.Sleep(30 * time.Millisecond)
	}

	// Kill the leader: node 1 is now a minority of one — no leader, no
	// commits.
	if err := s0.Close(); err != nil {
		t.Fatalf("close leader: %v", err)
	}
	waitForDegraded(t, s1)

	if _, ok := s1.DegradedSince(); !ok {
		t.Fatal("DegradedSince not set while degraded")
	}

	// Writes refused with KindUnavailable — and fast (no raft timeout).
	start := time.Now()
	if _, err := s1.Put(ctx, "/deg/k", []byte("v2")); !errors.Is(err, errors.KindUnavailable) {
		t.Fatalf("degraded put err = %v, want KindUnavailable", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("degraded write took %v; must be refused promptly", d)
	}
	// Txn routes through the same gate.
	if _, err := s1.Txn(ctx, []store.Op{{Kind: store.OpPut, Key: "/deg/x", Value: []byte("y")}}); !errors.Is(err, errors.KindUnavailable) {
		t.Errorf("degraded txn err = %v, want KindUnavailable", err)
	}

	// Stale reads still served from the local FSM.
	e, err := s1.Get(store.WithStale(ctx), "/deg/k")
	if err != nil {
		t.Fatalf("stale read: %v", err)
	}
	if string(e.Value) != "v1" {
		t.Errorf("stale read = %q, want last-known v1", e.Value)
	}
}

// TestDegradedRecoversOnQuorum: a degraded node returns to writable
// once a peer (and thus a leader) is reachable again — a restart of the
// old leader from its persistent state reforms the quorum.
func TestDegradedRecoversOnQuorum(t *testing.T) {
	s0, s1, restart0 := degradedRig(t)
	ctx := t.Context()

	if _, err := s0.Put(ctx, "/deg/k", []byte("v1")); err != nil {
		t.Fatalf("seed put: %v", err)
	}
	if err := s0.Close(); err != nil {
		t.Fatalf("close leader: %v", err)
	}
	waitForDegraded(t, s1)
	if _, err := s1.Put(ctx, "/deg/k", []byte("blocked")); !errors.Is(err, errors.KindUnavailable) {
		t.Fatalf("degraded write err = %v, want KindUnavailable", err)
	}

	// Leader returns from disk: quorum (2 of 2) is restored.
	s0 = restart0()
	waitLeaderSeenT(t, s0)
	waitLeaderSeenT(t, s1)

	deadline := time.Now().Add(10 * time.Second)
	for s1.Degraded() {
		if time.Now().After(deadline) {
			t.Fatal("node 1 still degraded after quorum returned")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Writable again: whichever node won the restarted election leads;
	// put and linearizable-read through it (the rig has no read
	// forwarder, so a follower cannot serve linearizable reads).
	lead := s1
	if s0.IsLeader() {
		lead = s0
	}
	if _, err := lead.Put(ctx, "/deg/k", []byte("v2")); err != nil {
		t.Fatalf("post-recovery put: %v", err)
	}
	e, err := lead.Get(ctx, "/deg/k")
	if err != nil {
		t.Fatalf("post-recovery read: %v", err)
	}
	if string(e.Value) != "v2" {
		t.Fatalf("post-recovery read = %q, want v2", e.Value)
	}
}

// TestDegradedHealthyClusterNotDegraded: the flag must stay false on a
// healthy 3-node cluster (no flapping during normal operation).
func TestDegradedHealthyClusterNotDegraded(t *testing.T) {
	c := NewTestCluster(t, 3)
	for i := 0; i < 5; i++ {
		if _, err := c.Leader().Put(t.Context(), store.Key("/deg/h"), []byte("x")); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	for i, n := range c.Nodes {
		if n.Degraded() {
			t.Fatalf("node %d of healthy cluster reported degraded", i)
		}
	}
}

// TestWitnessVotes (§4.9): a witness is a FULL raft voter — the record
// says witness, but raft-wise it votes and keeps quorum alive. Here the
// witness's vote keeps a 3-node cluster at quorum after a real member
// dies.
func TestWitnessVotes(t *testing.T) {
	c := NewTestCluster(t, 3)
	ctx := t.Context()

	// Node 2 is the witness: a full voter that the scheduler never places work on.
	rec := []byte(`{"id":"n2","raft_addr":"` + c.Nodes[2].Leader() + `","role":"witness","joined_at":1}`)
	if _, err := c.Leader().Put(ctx, store.Key("/nodes/n2"), rec); err != nil {
		t.Fatalf("write witness record: %v", err)
	}

	// Kill a REAL member: witness + survivor still form quorum (2 of 3).
	c.Kill(1)
	c.waitForLeader(0, 10*time.Second)
	if _, err := c.Nodes[0].Put(ctx, store.Key("/deg/after-witness"), []byte("ok")); err != nil {
		t.Fatalf("write after witness vote kept quorum: %v", err)
	}

	// Sanity: the witness node itself serves writes forwarded/committed.
	if _, err := c.Nodes[2].Put(ctx, store.Key("/deg/via-witness"), []byte("ok")); err != nil {
		t.Fatalf("write via witness: %v", err)
	}
}
