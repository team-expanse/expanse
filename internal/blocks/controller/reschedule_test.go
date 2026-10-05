package controller

// Rescheduling-on-node-failure tests (T17, §4.4).

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/scheduler"
	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/store/raftstore"
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

// statefulDB is a two-replica block with its own volume per replica, placed on three nodes.
func statefulDB(t *testing.T) (*Controller, *raftstore.Store, []scheduler.NodeView, *fakeClock) {
	t.Helper()
	b := blockFor("db", 2)
	b.Spec.Storage = append(b.Spec.Storage, &pb.Storage{Name: "data", Size: "1Gi"})
	st := newStore(t)
	mustCreate(t, context.Background(), st, b)
	nodes := nodeViews(3)
	c := fixedNodes(st, nodes)
	clk := setClock(c)
	reconcile(t, c)
	waitPlaced(t, context.Background(), c, "default", "db", 2)
	return c, st, nodes, clk
}

// holdCopy gives node a healthy copy of replica idx's volume.
func holdCopy(nodes []scheduler.NodeView, node string, idx int) {
	for i := range nodes {
		if nodes[i].ID == node {
			nodes[i].HealthyVolumes = append(nodes[i].HealthyVolumes, storage.BlockReplicaVolumeName("default", "db", "data", idx))
		}
	}
}

func setReady(nodes []scheduler.NodeView, node string, ready bool) {
	for i := range nodes {
		if nodes[i].ID == node {
			nodes[i].Ready = ready
		}
	}
}

// loseReplica0 takes down the node running replica 0 and lets the grace period pass.
func loseReplica0(t *testing.T, c *Controller, st *raftstore.Store, nodes []scheduler.NodeView, clk *fakeClock) string {
	t.Helper()
	down := placementAt(loadStatus(t, context.Background(), st, "default", "db"), 0).GetNodeId()
	setReady(nodes, down, false)
	reconcile(t, c)
	clk.Sleep(31 * time.Second)
	reconcile(t, c)
	return down
}

func otherNode(nodes []scheduler.NodeView, not ...string) string {
	for _, n := range nodes {
		if !slices.Contains(not, n.ID) {
			return n.ID
		}
	}
	return ""
}

// A stateful replica is not replaced until another reachable node holds a healthy copy of
// its volume: the replacement has nothing to run on otherwise.
func TestStatefulReplacementWaitsForAHealthyCopyElsewhere(t *testing.T) {
	c, st, nodes, clk := statefulDB(t)
	ctx := context.Background()
	down := placementAt(loadStatus(t, ctx, st, "default", "db"), 0).GetNodeId()
	holdCopy(nodes, down, 0) // the only copy is on the node that goes down
	loseReplica0(t, c, st, nodes, clk)

	s := loadStatus(t, ctx, st, "default", "db")
	if p := placementOn(s, down); p.GetPhase() != pb.Phase_LOST || p.GetReplicaIndex() != 0 {
		t.Fatalf("%s placement = %+v, want Lost and still replica 0", down, p)
	}
	if hasRetired(s) {
		t.Fatal("replica 0 retired with no healthy copy of its volume elsewhere")
	}
	if !strings.Contains(s.GetPendingReason().GetMessage(), "healthy copy") {
		t.Errorf("pending reason = %q, want it to say it waits for a healthy copy", s.GetPendingReason().GetMessage())
	}

	holdCopy(nodes, otherNode(nodes, down), 0)
	reconcile(t, c)
	reconcile(t, c)
	if repl := placementAt(loadStatus(t, ctx, st, "default", "db"), 0); repl == nil || repl.GetNodeId() == down {
		t.Errorf("replacement = %+v, want moved off %s once a copy exists elsewhere", repl, down)
	}
}

// A replica held for want of a copy runs again on its own node once that node returns.
func TestAHeldStatefulReplicaResumesWhenItsNodeReturns(t *testing.T) {
	c, st, nodes, clk := statefulDB(t)
	ctx := context.Background()
	down := loseReplica0(t, c, st, nodes, clk)
	if p := placementOn(loadStatus(t, ctx, st, "default", "db"), down); p.GetPhase() != pb.Phase_LOST {
		t.Fatalf("%s placement = %+v, want Lost", down, p)
	}

	setReady(nodes, down, true)
	reconcile(t, c)
	if p := placementAt(loadStatus(t, ctx, st, "default", "db"), 0); p.GetNodeId() != down || p.GetPhase() == pb.Phase_LOST {
		t.Errorf("replica 0 = %+v, want back on %s and no longer Lost", p, down)
	}
}

// A copy of another replica's volume, or one on a node that is itself down, does not count.
func TestStatefulReplacementIgnoresCopiesThatCannotServeIt(t *testing.T) {
	c, st, nodes, clk := statefulDB(t)
	ctx := context.Background()
	s0 := loadStatus(t, ctx, st, "default", "db")
	down, peer := placementAt(s0, 0).GetNodeId(), placementAt(s0, 1).GetNodeId()
	spare := otherNode(nodes, down, peer)
	holdCopy(nodes, peer, 1)
	holdCopy(nodes, spare, 0)
	setReady(nodes, spare, false)
	loseReplica0(t, c, st, nodes, clk)

	if s := loadStatus(t, ctx, st, "default", "db"); hasRetired(s) {
		t.Fatalf("replica 0 retired with no reachable copy of its volume: %v", s.GetPlacements())
	}
}

// A replica on a draining node stays put until its volume has a healthy copy elsewhere.
func TestDrainKeepsAStatefulReplicaWithoutACopyElsewhere(t *testing.T) {
	c, st, nodes, _ := statefulDB(t)
	ctx := context.Background()
	from := placementAt(loadStatus(t, ctx, st, "default", "db"), 0).GetNodeId()
	for i := range nodes {
		if nodes[i].ID == from {
			nodes[i].Ready, nodes[i].Cordoned, nodes[i].Draining = false, true, true
		}
	}
	reconcile(t, c)
	if p := placementAt(loadStatus(t, ctx, st, "default", "db"), 0); p.GetNodeId() != from {
		t.Fatalf("replica 0 = %+v, want kept on draining %s: no copy of its volume elsewhere", p, from)
	}

	holdCopy(nodes, otherNode(nodes, from), 0)
	reconcile(t, c)
	reconcile(t, c)
	if p := placementAt(loadStatus(t, ctx, st, "default", "db"), 0); p == nil || p.GetNodeId() == from {
		t.Errorf("replica 0 = %+v, want moved off draining %s", p, from)
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
