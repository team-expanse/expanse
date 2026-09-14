package controller

// Strategy tests (T16): singleton lease fencing (G4.11) and daemonset
// per-node placement.

import (
	"context"
	"testing"

	"github.com/expanse/expanse/internal/cluster/lease"
	"github.com/expanse/expanse/internal/scheduler"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
)

// singletonBlock is a singleton-strategy fixture (V5: replicas must be 1).
func singletonBlock(name string) *pb.Block {
	b := blockFor(name, 1)
	b.Spec.Strategy = &pb.Strategy{Kind: pb.StrategyKind_SINGLETON}
	return b
}

// daemonsetBlock is a daemonset fixture (V6: no replica count).
func daemonsetBlock(name string) *pb.Block {
	b := blockFor(name, 1)
	b.Spec.Replicas = nil
	b.Spec.Strategy = &pb.Strategy{Kind: pb.StrategyKind_DAEMONSET}
	return b
}

// G4.11: the singleton lease — not the replica count — enforces one
// placement. When the lease is live elsewhere (simulating a pre-heal
// leader that already scheduled), the controller must not place; once
// the lease is released, it places exactly one and holds the lease
// itself. A competing manager cannot then take the lease.
func TestSingletonLeaseGatesPlacement(t *testing.T) {
	ctx := context.Background()
	b := singletonBlock("single")
	st := newStore(t)
	mustCreate(t, ctx, st, b)
	c := New(st, func(context.Context) ([]scheduler.NodeView, scheduler.OvercommitConfig, error) {
		return nodeViews(3), testCfg(), nil
	})

	// A "partitioned leader" already holds the singleton lease.
	other := lease.NewManager(st, "other-node")
	h, err := other.TryAcquire(ctx, singletonLeaseName("default", "single"), lease.DefaultTTL)
	if err != nil {
		t.Fatalf("other-node acquire: %v", err)
	}

	if n, err := c.Reconcile(ctx); err != nil || n != 0 {
		t.Fatalf("Reconcile while lease held: n=%d err=%v", n, err)
	}
	s := loadStatus(t, ctx, st, "default", "single")
	if got := len(s.GetPlacements()); got != 0 {
		t.Fatalf("placed %d replicas while lease held elsewhere, want 0", got)
	}
	if s.GetPendingReason().GetCode() != "LeaseHeld" {
		t.Errorf("pending reason = %+v, want LeaseHeld", s.GetPendingReason())
	}

	// Release: the healed state. Controller now acquires and places.
	if err := other.Release(ctx, h); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	s = loadStatus(t, ctx, st, "default", "single")
	if got := len(s.GetPlacements()); got != 1 {
		t.Fatalf("placements after release = %d, want 1", got)
	}
	// The lease is now ours; a competing manager cannot take it.
	competitor := lease.NewManager(st, "competitor")
	if _, err := competitor.TryAcquire(ctx, singletonLeaseName("default", "single"), lease.DefaultTTL); err == nil {
		t.Fatal("competitor took the live singleton lease — split brain possible")
	}
}

// G4.11 chaos-style: two controllers racing to place after a heal. The
// loser's CAS must fail; only one placement may ever exist. Drive the
// race deterministically: pre-acquire (as if scheduled) then both
// managers TryAcquire — exactly one wins.
func TestSingletonDoublePlaceBlocked(t *testing.T) {
	ctx := context.Background()
	b := singletonBlock("racer")
	st := newStore(t)
	mustCreate(t, ctx, st, b)

	m1 := lease.NewManager(st, "node-1")
	m2 := lease.NewManager(st, "node-2")
	h1, err1 := m1.TryAcquire(ctx, singletonLeaseName("default", "racer"), lease.DefaultTTL)
	h2, err2 := m2.TryAcquire(ctx, singletonLeaseName("default", "racer"), lease.DefaultTTL)
	if (err1 == nil) == (err2 == nil) {
		t.Fatalf("both-or-neither acquired: err1=%v err2=%v (want exactly one winner)", err1, err2)
	}
	if h1 != nil {
		m1.Release(ctx, h1)
	}
	if h2 != nil {
		m2.Release(ctx, h2)
	}
}

// Daemonset: exactly one placement per Ready non-witness node in a
// 3-node fixture; a 4th node joining auto-extends; a cordoned node is
// still placed (cordon ignored per spec §4.4); a culled node's
// placement is removed with the stop hook fired.
func TestDaemonsetLifecycle(t *testing.T) {
	ctx := context.Background()
	b := daemonsetBlock("ds")
	st := newStore(t)
	mustCreate(t, ctx, st, b)

	nodeCount := 3
	c := New(st, func(context.Context) ([]scheduler.NodeView, scheduler.OvercommitConfig, error) {
		return nodeViews(nodeCount), testCfg(), nil
	})
	var stops int
	c.Update = &UpdateHooks{Stop: func(context.Context, *pb.Block, *pb.PlacementStatus) error {
		stops++
		return nil
	}}

	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	s := loadStatus(t, ctx, st, "default", "ds")
	if got := len(s.GetPlacements()); got != 3 {
		t.Fatalf("placements = %d, want 3 (one per node)", got)
	}
	seen := map[string]bool{}
	for _, p := range s.GetPlacements() {
		if seen[p.GetNodeId()] {
			t.Errorf("two placements on %s", p.GetNodeId())
		}
		seen[p.GetNodeId()] = true
	}

	// A 4th node joins: auto-extend on the next pass.
	nodeCount = 4
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	s = loadStatus(t, ctx, st, "default", "ds")
	if got := len(s.GetPlacements()); got != 4 {
		t.Fatalf("placements after join = %d, want 4", got)
	}
	newIdx := map[string]bool{}
	for _, p := range s.GetPlacements() {
		if p.GetReplicaIndex() >= 3 {
			newIdx[p.GetNodeId()] = true
		}
	}
	if len(newIdx) != 1 {
		t.Errorf("new node got %d placements, want exactly 1", len(newIdx))
	}

	// Cordoned nodes are still placed (cordon ignored by default):
	// drop the 4th node and add a cordoned one instead.
	nodeCount = 3
	nodes, _, _ := c.Nodes(ctx)
	nodes = append(nodes[:3:3], scheduler.NodeView{ID: "n4-cordoned", Ready: true, Cordoned: true})
	c.Nodes = func(context.Context) ([]scheduler.NodeView, scheduler.OvercommitConfig, error) {
		return nodes, testCfg(), nil
	}
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	s = loadStatus(t, ctx, st, "default", "ds")
	if got := len(s.GetPlacements()); got != 4 {
		t.Fatalf("placements with cordoned node = %d, want 4", got)
	}
	if placementOn(s, "n4") != nil {
		t.Error("old n4 placement not culled")
	}
	if placementOn(s, "n4-cordoned") == nil {
		t.Error("cordoned node not placed (cordon should be ignored)")
	}
	if stops != 1 {
		t.Errorf("stop hook fired %d times, want 1 (one cull)", stops)
	}
}

// A witness node never gets a daemonset placement.
func TestDaemonsetSkipsWitness(t *testing.T) {
	ctx := context.Background()
	b := daemonsetBlock("dsw")
	st := newStore(t)
	mustCreate(t, ctx, st, b)
	nodes := nodeViews(3)
	nodes[2].Witness = true
	c := New(st, func(context.Context) ([]scheduler.NodeView, scheduler.OvercommitConfig, error) {
		return nodes, testCfg(), nil
	})
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	s := loadStatus(t, ctx, st, "default", "dsw")
	if got := len(s.GetPlacements()); got != 2 {
		t.Fatalf("placements = %d, want 2 (witness skipped)", got)
	}
	if placementOn(s, "n3") != nil {
		t.Error("witness node got a placement")
	}
}

// Singleton rolling update still works through the lease: the replica
// is rolled in place while the lease is held by this controller.
func TestSingletonRollsThroughLease(t *testing.T) {
	ctx := context.Background()
	b := singletonBlock("single")
	st := newStore(t)
	mustCreate(t, ctx, st, b)
	c := New(st, func(context.Context) ([]scheduler.NodeView, scheduler.OvercommitConfig, error) {
		return nodeViews(2), testCfg(), nil
	})
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	waitPlaced(t, ctx, c, "default", "single", 1)

	// Spec update → new target generation; verify the roll completes and
	// never exceeds one placement.
	h := &updateHarness{clk: newFakeClock()}
	c.Update = h.hooks()
	spec, err := st.Get(ctx, blockKey("default", "single"))
	if err != nil {
		t.Fatal(err)
	}
	var blk pb.Block
	if err := proto.Unmarshal(spec.Value, &blk); err != nil {
		t.Fatal(err)
	}
	blk.Spec.Version = "v2"
	out, err := proto.Marshal(&blk)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Txn(ctx, []store.Op{
		{Kind: store.OpCheck, Key: spec.Key, Expect: spec.Revision},
		{Kind: store.OpPut, Key: spec.Key, Value: out},
	}); err != nil {
		t.Fatal(err)
	}
	status, _ := runRoll(t, ctx, c, h, "default", "single", 1, func() int { return 1 })
	if status.GetPhase() != pb.Phase_RUNNING {
		t.Fatalf("phase = %v, want RUNNING", status.GetPhase())
	}
	if got := len(status.GetPlacements()); got != 1 {
		t.Errorf("placements after singleton roll = %d, want 1", got)
	}
}
