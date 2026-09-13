package perf

import (
	"os"
	"testing"
)

// The cluster perf tests of §6. Gated behind RUN_PERF=1 like TestBudgets:
// they boot real in-process raft clusters and take a few minutes.
//
// Budgets (from budgets.yaml, single source of truth):
//
//	raft_write_p99_ms          ≤ 200   (1000 sequential writes, 3 nodes)
//	raft_read_linear_p99_ms    ≤ 50    (10,000 barrier reads)
//	raft_read_stale_p99_ms     ≤ 5     (10,000 stale reads)
//	cluster_form_3node_s       ≤ 30    (t0 → all three nodes led)
//	leader_election_p99_ms     ≤ 5000  (kill-leader rounds)
//	raft_snapshot_100k_s       ≤ 60    (force snapshot over 100k keys)
//	raft_restore_100k_s        ≤ 60    (fresh node via InstallSnapshot)

func loadMax(t *testing.T, name string) float64 {
	t.Helper()
	all := LoadBudgets()
	for _, b := range all {
		if b.Name == name {
			return b.Max
		}
	}
	t.Fatalf("budget %s missing from budgets.yaml", name)
	return 0
}

func assertBudget(t *testing.T, name string, measured float64) {
	t.Helper()
	max := loadMax(t, name)
	if max < 0 {
		t.Fatalf("budget %s not found", name)
	}
	if measured > max {
		t.Errorf("%s violated: %.2f > %.0f", name, measured, max)
	}
}

// TestRaftWriteLatency measures 1000 sequential writes on a 3-node
// cluster and asserts the p99 budget.
func TestRaftWriteLatency(t *testing.T) {
	if os.Getenv("RUN_PERF") == "" {
		t.Skip("set RUN_PERF=1 to run performance budget checks")
	}
	c, _ := newRaftCluster(3)
	defer c.Close()

	durs := c.writeLatencies(1000)
	p50, p99, p999 := percentile(durMs(durs), 0.50), percentile(durMs(durs), 0.99), percentile(durMs(durs), 0.999)
	t.Logf("1000 writes: p50=%.2fms p99=%.2fms p999=%.2fms", p50, p99, p999)
	assertBudget(t, "raft_write_p99_ms", p99)
}

// TestRaftReadLatency measures 10,000 linearizable and 10,000 stale
// reads and asserts both budgets.
func TestRaftReadLatency(t *testing.T) {
	if os.Getenv("RUN_PERF") == "" {
		t.Skip("set RUN_PERF=1 to run performance budget checks")
	}
	c, _ := newRaftCluster(3)
	defer c.Close()

	lin := percentile(durMs(c.readLatencies(10_000, false)), 0.99)
	t.Logf("10000 linearizable reads: p99=%.2fms", lin)
	assertBudget(t, "raft_read_linear_p99_ms", lin)

	stale := percentile(durMs(c.readLatencies(10_000, true)), 0.99)
	t.Logf("10000 stale reads: p99=%.2fms", stale)
	assertBudget(t, "raft_read_stale_p99_ms", stale)
}

// TestClusterFormation measures formation time for 3, 5 and 9 nodes.
func TestClusterFormation(t *testing.T) {
	if os.Getenv("RUN_PERF") == "" {
		t.Skip("set RUN_PERF=1 to run performance budget checks")
	}
	for _, n := range []int{3, 5, 9} {
		c, form := newRaftCluster(n)
		c.Close()
		t.Logf("formation %d nodes: %.2fs", n, form.Seconds())
		if n == 3 {
			assertBudget(t, "cluster_form_3node_s", form.Seconds())
		}
	}
}

// TestLeaderElection measures kill-leader election time over 6 rounds
// and asserts the p99 budget.
func TestLeaderElection(t *testing.T) {
	if os.Getenv("RUN_PERF") == "" {
		t.Skip("set RUN_PERF=1 to run performance budget checks")
	}
	c, _ := newRaftCluster(3)
	defer c.Close()

	durs := c.electionLatencies(6)
	for i, d := range durs {
		t.Logf("election round %d: %.2fs", i+1, d.Seconds())
	}
	p99 := percentile(durMs(durs), 0.99)
	t.Logf("leader election: p99=%.2fms over %d rounds", p99, len(durs))
	assertBudget(t, "leader_election_p99_ms", p99)
}

// TestSnapshotRestore100k writes 100k keys, forces a raft snapshot and
// times a fresh node's InstallSnapshot restore.
func TestSnapshotRestore100k(t *testing.T) {
	if os.Getenv("RUN_PERF") == "" {
		t.Skip("set RUN_PERF=1 to run performance budget checks")
	}
	c, _ := newRaftCluster(3)
	defer c.Close()

	snap, restore, keys := c.snapshotRestore100k()
	t.Logf("snapshot %d keys: %.2fs; restore: %.2fs", keys, snap.Seconds(), restore.Seconds())
	assertBudget(t, "raft_snapshot_100k_s", snap.Seconds())
	assertBudget(t, "raft_restore_100k_s", restore.Seconds())
}
