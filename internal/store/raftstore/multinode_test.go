package raftstore_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/conformance"
)

// TestConformanceThreeNodes: G3.2 — the raftstore passes the identical
// conformance suite at N=3. Every subtest gets a freshly formed 3-node
// cluster; the returned store is the current leader (writes may also be
// routed through followers via forwarding, but the conformance suite only
// touches the node it was handed).
func TestConformanceThreeNodes(t *testing.T) {
	conformance.RunConformance(t, func(t *testing.T) store.Store {
		c := NewTestCluster(t, 3)
		// Hand the suite the leader; the cluster's t.Cleanup tears the
		// remaining nodes down after the suite closes this one.
		return c.Leader()
	})
}

// TestLeaderKillsUnderLoad: G3.4 — with a 3-node cluster under continuous
// writes, killing the leader every 50 ops must preserve every acked write,
// keep revisions strictly monotonic, and re-elect within 10 s (spec §5).
func TestLeaderKillsUnderLoad(t *testing.T) {
	c := NewTestCluster(t, 3)

	const total = 150
	const killEvery = 50
	kills := 0
	maxRev := store.Revision(0)
	acked := map[store.Key][]byte{}

	for i := 0; i < total; i++ {
		if i > 0 && i%killEvery == 0 {
			// Kill the current leader; measure election time.
			li := -1
			for j, n := range c.Nodes {
				if n.IsLeader() {
					li = j
					break
				}
			}
			if li < 0 {
				t.Fatalf("op %d: no leader to kill", i)
			}
			killedAt := time.Now()
			c.Kill(li)

			// Election ≤ 10s: some surviving node becomes leader.
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				for j, n := range c.Nodes {
					if j != li && c.Nodes[j] != nil && n.IsLeader() {
						goto elected
					}
				}
				time.Sleep(20 * time.Millisecond)
			}
			t.Fatalf("no leader elected within 10s of killing node %d", li)
		elected:
			if d := time.Since(killedAt); d > 10*time.Second {
				t.Errorf("election took %v (> 10s)", d)
			}
			kills++

			// Bring the old leader back so the cluster returns to 3
			// voters before the next kill round.
			c.Restart(li)
		}

		k := store.Key(fmt.Sprintf("/load/%06d", i))
		v := []byte(fmt.Sprintf("v-%d", i))

		// Write through whatever node currently leads; rotate entry
		// points so both leader-local and follower-forwarded paths are
		// exercised.
		n := c.Nodes[i%len(c.Nodes)]
		rev, err := n.Put(t.Context(), k, v)
		if err != nil {
			// An error is acceptable only if the op was not committed;
			// we cannot observe that directly, so treat any error as
			// fatal — with quorum present these must not happen.
			t.Fatalf("put %d via node: %v", i, err)
		}
		if rev <= maxRev {
			t.Fatalf("put %d revision %d not > max acked %d", i, rev, maxRev)
		}
		maxRev = rev
		acked[k] = v
	}
	if kills == 0 {
		t.Fatal("no leader kills performed")
	}

	// Every acked write must be readable (linearizable) from the cluster.
	for k, v := range acked {
		e, err := c.Leader().Get(t.Context(), k)
		if err != nil {
			t.Fatalf("get %s after kills: %v", k, err)
		}
		if string(e.Value) != string(v) {
			t.Fatalf("get %s = %q, want %q", k, e.Value, v)
		}
	}

	// Full restart: shut every node down, revive all from their data
	// dirs — G3.5 durability signal.
	for i := range c.Nodes {
		c.Kill(i)
	}
	for i := range c.Nodes {
		c.start(i, false)
	}
	c.waitForLeader(0, 10*time.Second)

	for k, v := range acked {
		e, err := c.Leader().Get(t.Context(), k)
		if err != nil {
			t.Fatalf("get %s after full restart: %v", k, err)
		}
		if string(e.Value) != string(v) {
			t.Fatalf("get %s after full restart = %q, want %q", k, e.Value, v)
		}
	}
}
