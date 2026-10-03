package controller

// Rescheduling-on-node-failure tests (T17, §4.4).

import (
	"context"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/scheduler"
	pb "github.com/expanse/expanse/proto"
)

// setClock wires a fake clock for the §4.4 grace timing and returns it.
func setClock(c *Controller) *fakeClock {
	clk := newFakeClock()
	c.Update = &UpdateHooks{Now: clk.Now, Sleep: clk.Sleep}
	return clk
}

// statelessReplacement: after unreachable_grace, the placement on the
// failed node is marked Lost and a replacement is scheduled on a live
// node — not before the grace expires.
func TestStatelessRescheduleAfterGrace(t *testing.T) {
	ctx := context.Background()
	b := blockFor("web", 2)
	st := newStore(t)
	mustCreate(t, ctx, st, b)

	nodes := nodeViews(3)
	c := New(st, func(context.Context) ([]scheduler.NodeView, scheduler.OvercommitConfig, error) {
		return nodes, testCfg(), nil
	})
	clk := setClock(c)
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	waitPlaced(t, ctx, c, "default", "web", 2)

	// n1 goes down.
	nodes[0].Ready = false
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	s := loadStatus(t, ctx, st, "default", "web")
	if p := placementOn(s, "n1"); p.GetPhase() == pb.Phase_LOST {
		t.Error("marked Lost before the grace period expired")
	}

	// Grace expires (default 30 s): next pass marks Lost and replaces.
	clk.Sleep(31 * time.Second)
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	s = loadStatus(t, ctx, st, "default", "web")
	lost := placementOn(s, "n1")
	if lost.GetPhase() != pb.Phase_LOST || lost.GetReplicaIndex() != -1 {
		t.Errorf("n1 placement = %+v, want Lost and retired", lost)
	}
	repl := placementAt(s, 0)
	if repl == nil || repl.GetNodeId() != "n3" {
		t.Errorf("replacement for index 0 = %+v, want on n3", repl)
	}
}

// Stateful blocks wait for the StorageAvailable hook before a
// replacement is scheduled — the gate must actually block.
func TestStatefulRescheduleGatedOnStorageAvailable(t *testing.T) {
	ctx := context.Background()
	b := blockFor("db", 2)
	b.Spec.Storage = append(b.Spec.Storage, &pb.Storage{Name: "data", Size: "1Gi"})
	st := newStore(t)
	mustCreate(t, ctx, st, b)

	nodes := nodeViews(3)
	c := New(st, func(context.Context) ([]scheduler.NodeView, scheduler.OvercommitConfig, error) {
		return nodes, testCfg(), nil
	})
	clk := setClock(c)
	avail := false
	c.StorageAvailable = func(string) bool { return avail }
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	waitPlaced(t, ctx, c, "default", "db", 2)

	// Fail whichever node holds index 0, then burn the grace period.
	downID := placementAt(loadStatus(t, ctx, st, "default", "db"), 0).GetNodeId()
	for i := range nodes {
		if nodes[i].ID == downID {
			nodes[i].Ready = false
		}
	}
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	clk.Sleep(31 * time.Second)
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	s := loadStatus(t, ctx, st, "default", "db")
	if p := placementOn(s, downID); p.GetPhase() != pb.Phase_LOST {
		t.Errorf("n1 placement phase = %v, want Lost after grace", p.GetPhase())
	}
	if p := placementOn(s, downID); p.GetReplicaIndex() != 0 {
		t.Errorf("down-node index = %d, want still 0 (gate held: no replacement)", p.GetReplicaIndex())
	}
	for _, p := range s.GetPlacements() {
		if p.GetReplicaIndex() != -1 && p.GetNodeId() != downID && p.GetReplicaIndex() == 0 {
			t.Error("duplicate index-0 replacement while storage unavailable")
		}
	}
	if hasRetired(s) {
		t.Error("index retired while storage unavailable — gate did not block")
	}

	// Storage confirms availability: replacement proceeds.
	avail = true
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	s = loadStatus(t, ctx, st, "default", "db")
	if repl := placementAt(s, 0); repl == nil || repl.GetNodeId() == downID {
		t.Errorf("replacement after storage-available = %+v, want moved off %s", repl, downID)
	}
}

// Singleton replacement after node failure: exactly one active placement
// before and after; the old record is Lost and retired.
func TestSingletonReplacementNeverDoublePlaces(t *testing.T) {
	ctx := context.Background()
	b := singletonBlock("single")
	st := newStore(t)
	mustCreate(t, ctx, st, b)

	nodes := nodeViews(3)
	c := New(st, func(context.Context) ([]scheduler.NodeView, scheduler.OvercommitConfig, error) {
		return nodes, testCfg(), nil
	})
	clk := setClock(c)
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	waitPlaced(t, ctx, c, "default", "single", 1)

	downID := placementAt(loadStatus(t, ctx, st, "default", "single"), 0).GetNodeId()
	for i := range nodes {
		if nodes[i].ID == downID {
			nodes[i].Ready = false
		}
	}
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	clk.Sleep(31 * time.Second)
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	s := loadStatus(t, ctx, st, "default", "single")
	active := 0
	for _, p := range s.GetPlacements() {
		if p.GetReplicaIndex() != -1 {
			active++
		} else if p.GetPhase() != pb.Phase_LOST {
			t.Errorf("retired placement phase = %v, want Lost", p.GetPhase())
		}
	}
	if active != 1 {
		t.Errorf("active singleton placements = %d, want 1", active)
	}
	if placementOn(s, downID).GetReplicaIndex() != -1 {
		t.Error("old singleton placement not retired")
	}
}

// A cordoned node is healthy: cordon stops new placements, it never
// marks the node's replicas Lost or replaces them.
func TestCordonKeepsExistingReplicas(t *testing.T) {
	ctx := context.Background()
	b := blockFor("web", 2)
	st := newStore(t)
	mustCreate(t, ctx, st, b)

	nodes := nodeViews(3)
	c := New(st, func(context.Context) ([]scheduler.NodeView, scheduler.OvercommitConfig, error) {
		return nodes, testCfg(), nil
	})
	clk := setClock(c)
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	waitPlaced(t, ctx, c, "default", "web", 2)

	// The wire reports a cordoned node as not placeable (Ready=false).
	nodes[0].Ready, nodes[0].Cordoned = false, true
	for i := 0; i < 3; i++ {
		clk.Sleep(31 * time.Second)
		if _, err := c.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}
	s := loadStatus(t, ctx, st, "default", "web")
	if p := placementOn(s, "n1"); p.GetPhase() == pb.Phase_LOST || p.GetReplicaIndex() == -1 {
		t.Errorf("replica on cordoned n1 = %+v, want kept", p)
	}
}

// A draining node's replicas move on the next pass, no grace window.
func TestDrainMovesReplicasPromptly(t *testing.T) {
	ctx := context.Background()
	b := blockFor("web", 2)
	st := newStore(t)
	mustCreate(t, ctx, st, b)
	nodes := nodeViews(3)
	c := New(st, func(context.Context) ([]scheduler.NodeView, scheduler.OvercommitConfig, error) {
		return nodes, testCfg(), nil
	})
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	waitPlaced(t, ctx, c, "default", "web", 2)
	drained := placementAt(loadStatus(t, ctx, st, "default", "web"), 0).GetNodeId()
	for i := range nodes {
		if nodes[i].ID == drained {
			nodes[i].Ready, nodes[i].Cordoned, nodes[i].Draining = false, true, true
		}
	}
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	s := loadStatus(t, ctx, st, "default", "web")
	for _, p := range s.GetPlacements() {
		if p.GetNodeId() == drained && p.GetReplicaIndex() != -1 {
			t.Errorf("replica %d still on draining %s", p.GetReplicaIndex(), drained)
		}
	}
	if got := placementAt(s, 0).GetNodeId(); got == "" || got == drained {
		t.Errorf("replica 0 on %q, want a replacement off %s", got, drained)
	}
	if p := placementOn(s, drained); p.GetPhase() != pb.Phase_TERMINATED {
		t.Errorf("drained record phase = %v, want TERMINATED", p.GetPhase())
	}
}

// Drain stops a daemonset's replica too; cordon alone keeps it.
func TestDrainStopsDaemonsetReplica(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	mustCreate(t, ctx, st, daemonsetBlock("agent"))
	nodes := nodeViews(3)
	c := New(st, func(context.Context) ([]scheduler.NodeView, scheduler.OvercommitConfig, error) {
		return nodes, testCfg(), nil
	})
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	nodes[0].Ready, nodes[0].Cordoned, nodes[0].Draining = false, true, true
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if p := placementOn(loadStatus(t, ctx, st, "default", "agent"), "n1"); p != nil {
		t.Errorf("daemonset replica on draining n1 = %+v, want stopped", p)
	}
}
