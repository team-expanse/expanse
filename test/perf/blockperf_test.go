package perf

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/blocks/controller"
	"github.com/expanse/expanse/internal/quantity"
	"github.com/expanse/expanse/internal/scheduler"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/raftstore"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
)

// The block perf tests of §8. Gated behind RUN_PERF=1 like the rest of
// the suite. Budgets (from budgets.yaml, single source of truth):
//
//	schedule_100x3_ms          ≤ 500    (100 blocks × 3 replicas, 10 nodes — G4.4)
//	block_deploy_p99_s         ≤ 10     (spec accepted → RUNNING, cached closure — G4.5, in-proc)
//	agent_rss_per_block_bytes  ≤ 1 MiB  (resident overhead per block in expansed)
//	block_list_500_ms          ≤ 100    (List over 500 blocks in the store)

// newBlockStore boots a single-node raft store for block-perf tests.
func newBlockStore(t *testing.T) *raftstore.Store {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ln.Close()
	st, err := raftstore.Open(raftstore.Config{
		NodeID:    "n1",
		BindAddr:  ln.Addr().String(),
		DataDir:   t.TempDir(),
		Bootstrap: true,
		LogOutput: io.Discard,
	})
	if err != nil {
		t.Fatalf("raftstore.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && st.Leader() == "" {
		time.Sleep(20 * time.Millisecond)
	}
	if st.Leader() == "" {
		t.Fatal("no leader elected")
	}
	return st
}

// tenNodes returns 10 ready scheduler views with generous capacity.
func tenNodes() []scheduler.NodeView {
	nodes := make([]scheduler.NodeView, 10)
	for i := range nodes {
		nodes[i] = scheduler.NodeView{
			ID:           fmt.Sprintf("n%d", i),
			Ready:        true,
			FreeCPU:      quantity.CPU{Milli: 4000},
			FreeMem:      quantity.Bytes{N: 8 << 30},
			FreeDisk:     quantity.Bytes{N: 100 << 30},
			Capabilities: []string{"kvm"},
			Labels:       map[string]string{"zone": "a"},
			Arch:         "x86_64",
		}
	}
	return nodes
}

// echoBlock builds a minimal util/echo block with r replicas.
func echoBlock(name string, r int32) *pb.Block {
	return &pb.Block{
		Metadata: &pb.Metadata{Name: name, Namespace: "default"},
		Spec: &pb.BlockSpec{
			Type:      "util/echo",
			Replicas:  &r,
			Resources: &pb.Resources{Requests: &pb.ResourcePair{Cpu: "100m", Memory: "64Mi"}},
		},
	}
}

// TestBlockSchedule100x3 gates schedule_100x3_ms: placing 100 blocks ×
// 3 replicas across 10 simulated nodes takes ≤ 500 ms total (G4.4).
func TestBlockSchedule100x3(t *testing.T) {
	if os.Getenv("RUN_PERF") == "" {
		t.Skip("set RUN_PERF=1 to run performance budget checks")
	}
	nodes := tenNodes()
	cfg := scheduler.OvercommitConfig{CPUOvercommitRatio: 2.0, MemoryOvercommitRatio: 1.0}

	start := time.Now()
	for b := 0; b < 100; b++ {
		blk := echoBlock(fmt.Sprintf("b%03d", b), 3)
		// Round-robin prior placements across the ten nodes so the
		// anti-affinity filter exercises its existing-placement path.
		var existing []string
		for r := 0; r < 3; r++ {
			id, pending := scheduler.Schedule(nodes, scheduler.ReplicaRequest{
				Block:              blk,
				ReplicaIndex:       r,
				ExistingPlacements: existing,
			}, cfg, scheduler.ClusterView{})
			if pending != nil {
				t.Fatalf("block %d replica %d pending: %v", b, r, pending)
			}
			if id == "" {
				t.Fatalf("block %d replica %d: empty node id", b, r)
			}
			existing = append(existing, id)
		}
	}
	ms := float64(time.Since(start).Milliseconds())
	t.Logf("100 blocks × 3 replicas on 10 nodes: %.0f ms", ms)
	assertBudget(t, "schedule_100x3_ms", ms)
}

// TestBlockDeployLatency gates block_deploy_p99_s: the full in-process
// deploy path — spec accepted (block Put) → controller placement →
// simulated agent health report → controller promotion to RUNNING —
// with everything warm ("cached closure"), p99 ≤ 10 s (G4.5).
func TestBlockDeployLatency(t *testing.T) {
	if os.Getenv("RUN_PERF") == "" {
		t.Skip("set RUN_PERF=1 to run performance budget checks")
	}
	ctx := context.Background()
	st := newBlockStore(t)
	c := controller.New(st, func(context.Context) ([]scheduler.NodeView, scheduler.OvercommitConfig, error) {
		return tenNodes(), scheduler.OvercommitConfig{CPUOvercommitRatio: 2.0, MemoryOvercommitRatio: 1.0}, nil
	})
	c.Interval = time.Hour // manual Reconcile driving only

	var durs []float64
	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("deploy-%02d", i)
		blk := echoBlock(name, 1)
		raw, err := proto.Marshal(blk)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		start := time.Now()
		if _, err := st.Put(ctx, store.Key("/blocks/default/"+name), raw); err != nil {
			t.Fatalf("Put: %v", err)
		}
		// Pass 1: places the replica (SCHEDULING) — the "cached
		// closure" analogue: unit materialization is simulated warm.
		if _, err := c.Reconcile(ctx); err != nil {
			t.Fatalf("reconcile 1: %v", err)
		}
		// Simulated agent health report for the placed replica (node
		// read from the placement the first pass persisted).
		se1, err := st.Get(ctx, store.Key("/blocks/default/"+name+"/status"))
		if err != nil {
			t.Fatalf("status after pass 1: %v", err)
		}
		var st1 pb.BlockStatus
		if err := proto.Unmarshal(se1.Value, &st1); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(st1.GetPlacements()) != 1 {
			t.Fatalf("%s: %d placements after pass 1", name, len(st1.GetPlacements()))
		}
		node := st1.GetPlacements()[0].GetNodeId()
		if _, err := st.Put(ctx, store.Key(
			"/node/"+node+"/status/resources/"+controller.ReplicaResourceID("default", name, 0)),
			[]byte("health=healthy in_sync=true")); err != nil {
			t.Fatalf("agent report: %v", err)
		}
		// Pass 2: RuntimePass promotes to RUNNING.
		if _, err := c.Reconcile(ctx); err != nil {
			t.Fatalf("reconcile 2: %v", err)
		}
		se, err := st.Get(ctx, store.Key("/blocks/default/"+name+"/status"))
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		var status pb.BlockStatus
		if err := proto.Unmarshal(se.Value, &status); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if status.GetPhase() != pb.Phase_RUNNING {
			t.Fatalf("%s did not reach RUNNING (phase %v)", name, status.GetPhase())
		}
		durs = append(durs, time.Since(start).Seconds())
		// Clean up for the next iteration keeps placements disjoint.
		if _, err := c.Reconcile(ctx); err != nil {
			t.Fatalf("reconcile 3: %v", err)
		}
	}
	p99 := percentile(durs, 0.99)
	t.Logf("deploy to RUNNING (10 deploys): p99=%.3fs", p99)
	assertBudget(t, "block_deploy_p99_s", p99)
}

// TestBlockList500 gates block_list_500_ms: 500 blocks resident in the
// store must list in ≤ 100 ms.
func TestBlockList500(t *testing.T) {
	if os.Getenv("RUN_PERF") == "" {
		t.Skip("set RUN_PERF=1 to run performance budget checks")
	}
	ctx := context.Background()
	st := newBlockStore(t)
	for i := 0; i < 500; i++ {
		raw, err := proto.Marshal(echoBlock(fmt.Sprintf("b%03d", i), 3))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := st.Put(ctx, store.Key(fmt.Sprintf("/blocks/default/b%03d", i)), raw); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	var worst time.Duration
	for round := 0; round < 10; round++ {
		start := time.Now()
		entries, err := st.List(ctx, "/blocks/")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(entries) < 500 {
			t.Fatalf("List returned %d entries, want ≥ 500", len(entries))
		}
		if d := time.Since(start); d > worst {
			worst = d
		}
	}
	ms := float64(worst.Milliseconds())
	t.Logf("worst of 10 lists over 500 blocks: %.0f ms", ms)
	assertBudget(t, "block_list_500_ms", ms)
}

// TestAgentRSSPerBlock gates agent_rss_per_block_bytes: the resident
// heap cost of holding 500 blocks (+ statuses) in an expansed process
// stays ≤ 1 MiB per block.
func TestAgentRSSPerBlock(t *testing.T) {
	if os.Getenv("RUN_PERF") == "" {
		t.Skip("set RUN_PERF=1 to run performance budget checks")
	}
	ctx := context.Background()
	st := newBlockStore(t)

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	for i := 0; i < 500; i++ {
		name := fmt.Sprintf("b%03d", i)
		raw, err := proto.Marshal(echoBlock(name, 3))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := st.Put(ctx, store.Key("/blocks/default/"+name), raw); err != nil {
			t.Fatalf("Put: %v", err)
		}
		// A realistic status with 3 placements each.
		status := &pb.BlockStatus{Phase: pb.Phase_RUNNING}
		for r := 0; r < 3; r++ {
			status.Placements = append(status.Placements, &pb.PlacementStatus{
				ReplicaIndex: int32(r),
				NodeId:       fmt.Sprintf("n%d", r),
				Phase:        pb.Phase_RUNNING,
				Generation:   1,
			})
		}
		sraw, err := proto.Marshal(status)
		if err != nil {
			t.Fatalf("marshal status: %v", err)
		}
		if _, err := st.Put(ctx, store.Key("/blocks/default/"+name+"/status"), sraw); err != nil {
			t.Fatalf("Put status: %v", err)
		}
	}

	// Pull everything through the read path like a listing expansed.
	entries, err := st.List(ctx, "/blocks/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) < 1000 {
		t.Fatalf("List returned %d entries, want ≥ 1000", len(entries))
	}
	var keep [][]byte // hold decoded forms alive for the measurement
	for _, e := range entries {
		keep = append(keep, e.Value)
	}

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(keep)
	perBlock := float64(after.HeapAlloc-before.HeapAlloc) / 500
	t.Logf("heap growth holding 500 blocks+statuses: %.0f bytes/block", perBlock)
	assertBudget(t, "agent_rss_per_block_bytes", perBlock)
}
