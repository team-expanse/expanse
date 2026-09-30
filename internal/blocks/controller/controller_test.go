package controller

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/quantity"
	"github.com/expanse/expanse/internal/scheduler"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/raftstore"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
)

// nodeViews builds n ready simulated nodes with ample resources.
func nodeViews(n int) []scheduler.NodeView {
	out := make([]scheduler.NodeView, n)
	for i := range out {
		out[i] = scheduler.NodeView{
			ID:       fmt.Sprintf("n%d", i+1),
			Ready:    true,
			FreeCPU:  quantity.CPU{Milli: 4000},
			FreeMem:  quantity.Bytes{N: 8 << 30},
			FreeDisk: quantity.Bytes{N: 100 << 30},
		}
	}
	return out
}

func testCfg() scheduler.OvercommitConfig {
	return scheduler.OvercommitConfig{CPUOvercommitRatio: 2.0, MemoryOvercommitRatio: 1.0}
}

// blockFor builds an anti-affinity block with r replicas.
func blockFor(name string, r int32) *pb.Block {
	i := r
	return &pb.Block{
		Metadata: &pb.Metadata{Name: name, Namespace: "default"},
		Spec: &pb.BlockSpec{
			Type:     "util/echo",
			Replicas: &i,
			Placement: &pb.Placement{
				AntiAffinity: pb.AntiAffinity_ANTI_AFFINITY_NODE,
			},
			Resources: &pb.Resources{Requests: &pb.ResourcePair{Cpu: "100m", Memory: "64Mi"}},
		},
	}
}

func newStore(t *testing.T) *raftstore.Store {
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
		t.Fatalf("Open: %v", err)
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

func mustCreate(t *testing.T, ctx context.Context, st *raftstore.Store, b *pb.Block) {
	t.Helper()
	out, err := proto.Marshal(b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := st.Txn(ctx, []store.Op{{Kind: store.OpPut, Key: blockKey(
		b.GetMetadata().GetNamespace(), b.GetMetadata().GetName()), Value: out}}); err != nil {
		t.Fatalf("put block: %v", err)
	}
}

func loadStatus(t *testing.T, ctx context.Context, st *raftstore.Store, ns, name string) *pb.BlockStatus {
	t.Helper()
	e, err := st.Get(ctx, statusKey(blockKey(ns, name)))
	if err != nil {
		return nil
	}
	var s pb.BlockStatus
	if err := proto.Unmarshal(e.Value, &s); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	return &s
}

// waitPlaced polls until the block has want placements or times out.
func waitPlaced(t *testing.T, ctx context.Context, c *Controller, ns, name string, want int) *pb.BlockStatus {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s := loadStatus(t, ctx, c.St, ns, name)
		if s != nil && len(s.GetPlacements()) == want {
			return s
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("block %s/%s never reached %d placements; status: %+v", ns, name, want,
		loadStatus(t, ctx, c.St, ns, name))
	return nil
}

// G4.2: 3 replicas on 3 distinct nodes with anti-affinity node.
func TestThreeReplicasThreeDistinctNodes(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	mustCreate(t, ctx, st, blockFor("web", 3))
	c := New(st, func(context.Context) ([]scheduler.NodeView, scheduler.OvercommitConfig, error) {
		return nodeViews(3), testCfg(), nil
	})
	if n, err := c.Reconcile(ctx); err != nil || n != 3 {
		t.Fatalf("Reconcile placed %d (err %v), want 3", n, err)
	}
	s := waitPlaced(t, ctx, c, "default", "web", 3)
	seen := map[string]bool{}
	for _, p := range s.GetPlacements() {
		if seen[p.GetNodeId()] {
			t.Errorf("two replicas on node %s", p.GetNodeId())
		}
		seen[p.GetNodeId()] = true
		if p.GetPhase() != pb.Phase_SCHEDULING {
			t.Errorf("placement phase = %v, want SCHEDULING", p.GetPhase())
		}
	}
	if s.GetPhase() != pb.Phase_SCHEDULING {
		t.Errorf("block phase = %v, want SCHEDULING", s.GetPhase())
	}
	if s.GetPendingReason() != nil {
		t.Errorf("unexpected pending reason: %+v", s.GetPendingReason())
	}
}

// G4.3: 4 replicas on 3 nodes, anti-affinity node → 3 placed, 1 Pending
// with AntiAffinityConflict reason.
func TestFourReplicasThreeNodesPendingReason(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	mustCreate(t, ctx, st, blockFor("web", 4))
	c := New(st, func(context.Context) ([]scheduler.NodeView, scheduler.OvercommitConfig, error) {
		return nodeViews(3), testCfg(), nil
	})
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	s := waitPlaced(t, ctx, c, "default", "web", 3)
	if s.GetPhase() != pb.Phase_PENDING {
		t.Errorf("block phase = %v, want PENDING", s.GetPhase())
	}
	pr := s.GetPendingReason()
	if pr == nil || pr.GetCode() != scheduler.CodeAntiAffinity {
		t.Errorf("pending reason = %+v, want AntiAffinityConflict", pr)
	}
	if len(pr.GetPerNode()) == 0 {
		t.Error("PendingReason.PerNode is empty — §4.3 requires per-node breakdown")
	}
	// The 4th replica must NOT be recorded as placed.
	for _, p := range s.GetPlacements() {
		if p.GetReplicaIndex() == 3 {
			t.Error("4th replica placed despite anti-affinity conflict")
		}
	}
}

// The 30 s timer independently re-places a previously pending replica.
func TestTimerTriggerReplacesPending(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	mustCreate(t, ctx, st, blockFor("web", 3))

	var mu sync.Mutex
	nodes := nodeViews(3)
	for i := range nodes { // start: nothing ready → everything pending
		nodes[i].Ready = false
	}
	c := New(st, func(context.Context) ([]scheduler.NodeView, scheduler.OvercommitConfig, error) {
		mu.Lock()
		defer mu.Unlock()
		return append([]scheduler.NodeView(nil), nodes...), testCfg(), nil
	})
	c.Interval = 20 * time.Millisecond
	if n, _ := c.Reconcile(ctx); n != 0 {
		t.Fatalf("placed %d with no ready nodes", n)
	}
	if s := loadStatus(t, ctx, st, "default", "web"); s == nil || s.GetPendingReason() == nil {
		t.Fatalf("expected pending status with reason, got %+v", s)
	}

	mu.Lock()
	for i := range nodes {
		nodes[i].Ready = true
	}
	mu.Unlock()

	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(runCtx); close(done) }()
	waitPlaced(t, ctx, c, "default", "web", 3)
	cancel()
	<-done
}

// A node-join callback independently triggers re-placement of a pending
// replica — without waiting for the timer.
func TestNodeJoinCallbackReplacesPending(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	mustCreate(t, ctx, st, blockFor("web", 3))

	var mu sync.Mutex
	nodes := nodeViews(2) // cluster starts with 2 nodes: 3rd replica pending
	c := New(st, func(context.Context) ([]scheduler.NodeView, scheduler.OvercommitConfig, error) {
		mu.Lock()
		defer mu.Unlock()
		return append([]scheduler.NodeView(nil), nodes...), testCfg(), nil
	})
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if s := loadStatus(t, ctx, st, "default", "web"); s == nil || s.GetPendingReason() == nil {
		t.Fatalf("expected pending status, got %+v", s)
	}

	mu.Lock()
	nodes = nodeViews(3) // n3 joins
	mu.Unlock()

	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(runCtx); close(done) }()
	defer func() { cancel(); <-done }()
	c.NotifyNodeJoin("n3") // §4.3 trigger — must not rely on the timer
	s := waitPlaced(t, ctx, c, "default", "web", 3)
	if s.GetPendingReason() != nil {
		t.Errorf("pending reason survived placement: %+v", s.GetPendingReason())
	}
	if c.NotifyCount("node-join:n3") != 1 {
		t.Errorf("node-join notify count = %d, want 1", c.NotifyCount("node-join:n3"))
	}
}

// The other triggers also wake the loop; assert they count and wake.
func TestOtherTriggerCallbacks(t *testing.T) {
	st := newStore(t)
	c := New(st, func(context.Context) ([]scheduler.NodeView, scheduler.OvercommitConfig, error) {
		return nil, testCfg(), nil
	})
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(runCtx); close(done) }()
	time.Sleep(10 * time.Millisecond)

	c.NotifyUncordon("n1")
	c.NotifyBlockDelete("default", "web")
	c.NotifyResourceRelease("n2")
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	for _, kind := range []string{"uncordon:n1", "block-delete:default/web", "resource-release:n2"} {
		if c.NotifyCount(kind) < 1 {
			t.Errorf("trigger %q never fired", kind)
		}
	}
}

// Blocks placed in one pass see each other's load, so they spread instead of piling up.
func TestBlocksPlacedInOnePassSpreadAcrossNodes(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	c := New(st, func(context.Context) ([]scheduler.NodeView, scheduler.OvercommitConfig, error) {
		views := nodeViews(4)
		for i := range views {
			views[i].CapacityCPU = quantity.CPU{Milli: 4000}
		}
		return views, testCfg(), nil
	})
	names := []string{"web1", "web2", "web3", "web4"}
	for _, name := range names {
		mustCreate(t, ctx, st, blockFor(name, 1))
	}
	if n, err := c.Reconcile(ctx); err != nil || n != 4 {
		t.Fatalf("Reconcile = %d, %v; want 4 placed", n, err)
	}
	used := map[string]string{}
	for _, name := range names {
		node := loadStatus(t, ctx, st, "default", name).GetPlacements()[0].GetNodeId()
		if other, taken := used[node]; taken {
			t.Errorf("%s and %s both placed on %s", other, name, node)
		}
		used[node] = name
	}
}
