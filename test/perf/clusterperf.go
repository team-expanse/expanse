package perf

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/raftstore"
)

// raftCluster is a minimal in-process raftstore cluster for latency
// measurement (no fault injection — just the stores on loopback).
type raftCluster struct {
	nodes []*raftstore.Store
	dirs  []string
	addrs []string
}

func freeTCPPort() int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// newRaftCluster starts n raft nodes, bootstraps the first, joins the
// rest via AddVoter, and waits until every node sees a leader. Returns
// the time from the first Open call to full formation.
func newRaftCluster(n int) (*raftCluster, time.Duration) {
	c := &raftCluster{nodes: make([]*raftstore.Store, n), dirs: make([]string, n), addrs: make([]string, n)}
	for i := 0; i < n; i++ {
		d, err := os.MkdirTemp("", "expanse-perf-raft-*")
		if err != nil {
			c.Close()
			return nil, 0
		}
		c.dirs[i] = d
		c.addrs[i] = fmt.Sprintf("127.0.0.1:%d", freeTCPPort())
	}
	start := time.Now()
	open := func(i int, bootstrap bool) error {
		s, err := raftstore.Open(raftstore.Config{
			NodeID:    fmt.Sprintf("n%d", i),
			BindAddr:  c.addrs[i],
			DataDir:   c.dirs[i],
			Bootstrap: bootstrap,
		})
		if err != nil {
			return err
		}
		c.nodes[i] = s
		return nil
	}
	if err := open(0, true); err != nil {
		c.Close()
		return nil, 0
	}
	c.waitLeader(0, 30*time.Second)
	for i := 1; i < n; i++ {
		if err := open(i, false); err != nil {
			c.Close()
			return nil, 0
		}
		if err := c.nodes[0].AddVoter(fmt.Sprintf("n%d", i), c.addrs[i]); err != nil {
			c.Close()
			return nil, 0
		}
	}
	for i := 0; i < n; i++ {
		c.waitLeader(i, 30*time.Second)
	}
	return c, time.Since(start)
}

// waitLeader polls until node i reports a raft leader.
func (c *raftCluster) waitLeader(i int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s := c.nodes[i]; s != nil && s.Leader() != "" {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	panic(fmt.Sprintf("perf cluster: n%d no leader within %v", i, timeout))
}

// leader returns the current leader store (nil if unknown).
func (c *raftCluster) leader() *raftstore.Store {
	for _, s := range c.nodes {
		if s != nil && s.IsLeader() {
			return s
		}
	}
	return nil
}

// Close shuts every live node down (data dirs are removed).
func (c *raftCluster) Close() {
	for i, s := range c.nodes {
		if s != nil {
			_ = s.Close()
			c.nodes[i] = nil
		}
	}
	for _, d := range c.dirs {
		if d != "" {
			_ = os.RemoveAll(d)
		}
	}
}

// restart reopens node i on its existing data dir and raft address
// (crash-restart with state preserved).
func (c *raftCluster) restart(i int) error {
	s, err := raftstore.Open(raftstore.Config{
		NodeID:    fmt.Sprintf("n%d", i),
		BindAddr:  c.addrs[i],
		DataDir:   c.dirs[i],
		Bootstrap: false,
	})
	if err != nil {
		return err
	}
	c.nodes[i] = s
	return nil
}

// --- measurements ---------------------------------------------------------

// writeLatencies performs n sequential puts through the leader and
// returns the per-op durations.
func (c *raftCluster) writeLatencies(n int) []time.Duration {
	lead := c.leader()
	if lead == nil {
		panic("perf cluster: no leader for writes")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	out := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		k := store.Key(fmt.Sprintf("perf/w/%08d", i))
		start := time.Now()
		if _, err := lead.Put(ctx, k, []byte(fmt.Sprintf("v-%d", i))); err != nil {
			panic(fmt.Sprintf("perf write %d: %v", i, err))
		}
		out = append(out, time.Since(start))
	}
	return out
}

// readLatencies performs n reads of one warm key in the given mode.
func (c *raftCluster) readLatencies(n int, stale bool) []time.Duration {
	lead := c.leader()
	if lead == nil {
		panic("perf cluster: no leader for reads")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if stale {
		ctx = store.WithStale(ctx)
	}
	k := store.Key("perf/reads")
	if _, err := lead.Put(context.Background(), k, []byte("warm")); err != nil {
		panic(err)
	}
	out := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		start := time.Now()
		if _, err := lead.Get(ctx, k); err != nil {
			panic(fmt.Sprintf("perf read %d: %v", i, err))
		}
		out = append(out, time.Since(start))
	}
	return out
}

// electionLatencies performs rounds of leader-kill elections: closes the
// current leader, measures the time until a follower wins, restarts the
// old leader, and repeats. Returns per-round durations.
func (c *raftCluster) electionLatencies(rounds int) []time.Duration {
	out := make([]time.Duration, 0, rounds)
	for r := 0; r < rounds; r++ {
		leadIdx := -1
		for i, s := range c.nodes {
			if s != nil && s.IsLeader() {
				leadIdx = i
				break
			}
		}
		if leadIdx < 0 {
			panic("perf cluster: no leader to kill")
		}
		start := time.Now()
		if err := c.nodes[leadIdx].Close(); err != nil {
			panic(err)
		}
		c.nodes[leadIdx] = nil
		// Wait for a follower to win.
		won := false
		deadline := start.Add(30 * time.Second)
		for time.Now().Before(deadline) {
			for i, s := range c.nodes {
				if i != leadIdx && s != nil && s.IsLeader() {
					won = true
					break
				}
			}
			if won {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !won {
			panic("perf cluster: no successor after leader close")
		}
		out = append(out, time.Since(start))
		// Bring the old leader back for the next round.
		if err := c.restart(leadIdx); err != nil {
			panic(err)
		}
		c.waitLeader(leadIdx, 30*time.Second)
		time.Sleep(500 * time.Millisecond) // let the catch-up settle
	}
	return out
}

// snapshotRestore100k writes 100k keys (batched 100 per txn — the FSM
// state size is what the snapshot measures), forces a raft snapshot on
// the leader, then times a fresh node restoring via InstallSnapshot.
func (c *raftCluster) snapshotRestore100k() (snapshot, restore time.Duration, keys int) {
	lead := c.leader()
	if lead == nil {
		panic("perf cluster: no leader for snapshot")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	const total, batch = 100_000, 100
	last := ""
	for i := 0; i < total; i += batch {
		ops := make([]store.Op, 0, batch)
		for j := 0; j < batch; j++ {
			last = fmt.Sprintf("perf/snap/%06d", i+j)
			ops = append(ops, store.Op{Kind: store.OpPut, Key: store.Key(last), Value: []byte("payload-0123456789")})
		}
		if _, err := lead.Txn(ctx, ops); err != nil {
			panic(err)
		}
	}
	keys = total

	// Force the raft snapshot (log is long past SnapshotThreshold).
	start := time.Now()
	if err := lead.Snapshot(); err != nil {
		panic(err)
	}
	snapshot = time.Since(start)

	// Restore: open a fresh node on an empty dir; it must catch up via
	// InstallSnapshot. Time from AddVoter until the last key is visible
	// in its FSM.
	freshIdx := len(c.nodes)
	d, err := os.MkdirTemp("", "expanse-perf-restore-*")
	if err != nil {
		panic(err)
	}
	c.dirs = append(c.dirs, d)
	c.addrs = append(c.addrs, fmt.Sprintf("127.0.0.1:%d", freeTCPPort()))
	fresh, err := raftstore.Open(raftstore.Config{
		NodeID:   fmt.Sprintf("n%d", freshIdx),
		BindAddr: c.addrs[freshIdx],
		DataDir:  d,
	})
	if err != nil {
		panic(err)
	}
	c.nodes = append(c.nodes, fresh)
	rStart := time.Now()
	if err := lead.AddVoter(fmt.Sprintf("n%d", freshIdx), c.addrs[freshIdx]); err != nil {
		panic(err)
	}
	deadline := rStart.Add(120 * time.Second)
	dbg := os.Getenv("PERF_DEBUG") != ""
	nextLog := time.Now()
	for time.Now().Before(deadline) {
		// Stale read: inspect the fresh node's local FSM directly (what
		// we are measuring). A linearizable read would barrier through
		// the leader and say nothing about the local restore.
		e, err := fresh.Get(store.WithStale(ctx), store.Key(last))
		if err == nil && e != nil {
			break
		}
		if dbg && time.Now().After(nextLog) {
			nextLog = time.Now().Add(2 * time.Second)
			fmt.Printf("restore-debug: leader=%q stale-get-err=%v\n", fresh.Leader(), err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	restore = time.Since(rStart)
	return snapshot, restore, keys
}

// --- statistics helpers ----------------------------------------------------

func durMs(durs []time.Duration) []float64 {
	out := make([]float64, len(durs))
	for i, d := range durs {
		out[i] = float64(d.Microseconds()) / 1000.0
	}
	return out
}
