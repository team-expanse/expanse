package controller

// Liveness rescheduling tests: a node that gave up restarting a replica gets it moved elsewhere.

import (
	"context"
	"testing"

	"github.com/expanse/expanse/internal/blocks/health"
	"github.com/expanse/expanse/internal/scheduler"
	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/raftstore"
	pb "github.com/expanse/expanse/proto"
)

// publishLiveness writes node's liveness record for replica idx of default/name.
func publishLiveness(t *testing.T, st *raftstore.Store, name string, idx int32, node string, rec health.LivenessRecord) {
	t.Helper()
	if err := health.LivenessPublisher(st, blockKey("default", name), idx, node)(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
}

// fixedNodes builds a controller over a fixed node view.
func fixedNodes(st *raftstore.Store, nodes []scheduler.NodeView) *Controller {
	return New(st, func(context.Context) ([]scheduler.NodeView, scheduler.OvercommitConfig, error) {
		return nodes, testCfg(), nil
	})
}

func reconcile(t *testing.T, c *Controller) {
	t.Helper()
	if _, err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// failedPlacementOn finds the retired FAILED record left on node.
func failedPlacementOn(s *pb.BlockStatus, node string) *pb.PlacementStatus {
	for _, p := range s.GetPlacements() {
		if p.GetNodeId() == node && p.GetPhase() == pb.Phase_FAILED {
			return p
		}
	}
	return nil
}

func TestLivenessFailureMovesReplicaOffItsNode(t *testing.T) {
	ctx := context.Background()
	b := blockFor("web", 1)
	b.Spec.Placement = nil // nothing but the failure keeps it off its node
	st := newStore(t)
	mustCreate(t, ctx, st, b)
	c := fixedNodes(st, nodeViews(3))
	reconcile(t, c)
	from := placementAt(waitPlaced(t, ctx, c, "default", "web", 1), 0).GetNodeId()

	publishLiveness(t, st, "web", 0, from, health.LivenessRecord{Restarts: 5, Failed: true, Detail: "connection refused"})
	reconcile(t, c)
	reconcile(t, c)

	s := loadStatus(t, ctx, st, "default", "web")
	if old := failedPlacementOn(s, from); old == nil || old.GetReplicaIndex() != -1 {
		t.Fatalf("placement on %s = %+v, want retired and FAILED (status %v)", from, old, s)
	}
	if m := failedPlacementOn(s, from).GetMessage(); m != "liveness probe failed after 5 restarts: connection refused" {
		t.Errorf("failed placement message = %q", m)
	}
	if p := placementAt(s, 0); p == nil || p.GetNodeId() == from {
		t.Fatalf("replica 0 = %+v, want it on a node other than %s", p, from)
	}
}

func TestLivenessFailureNeverReturnsToTheFailedNode(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	mustCreate(t, ctx, st, blockFor("web", 1))
	c := fixedNodes(st, nodeViews(1))
	reconcile(t, c)
	waitPlaced(t, ctx, c, "default", "web", 1)

	publishLiveness(t, st, "web", 0, "n1", health.LivenessRecord{Failed: true})
	reconcile(t, c)
	reconcile(t, c)

	s := loadStatus(t, ctx, st, "default", "web")
	if p := placementAt(s, 0); p != nil {
		t.Fatalf("replica 0 placed back on %s after failing there", p.GetNodeId())
	}
	if s.GetPhase() != pb.Phase_PENDING {
		t.Fatalf("phase = %v, want PENDING with no other node", s.GetPhase())
	}
}

func TestLivenessUnknownOrRestartingLeavesReplicaAlone(t *testing.T) {
	cases := map[string]func(t *testing.T, st *raftstore.Store, node string){
		"no record": func(*testing.T, *raftstore.Store, string) {},
		"still restarting": func(t *testing.T, st *raftstore.Store, node string) {
			publishLiveness(t, st, "web", 0, node, health.LivenessRecord{Restarts: 4})
		},
		"another node's verdict": func(t *testing.T, st *raftstore.Store, node string) {
			publishLiveness(t, st, "web", 0, "elsewhere", health.LivenessRecord{Failed: true})
		},
		"garbled record": func(t *testing.T, st *raftstore.Store, _ string) {
			if _, err := st.Put(context.Background(), store.Key("/blocks/default/web/status/liveness/0"), []byte("{")); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			st := newStore(t)
			mustCreate(t, ctx, st, blockFor("web", 1))
			c := fixedNodes(st, nodeViews(2))
			reconcile(t, c)
			node := placementAt(waitPlaced(t, ctx, c, "default", "web", 1), 0).GetNodeId()
			setup(t, st, node)
			reconcile(t, c)
			s := loadStatus(t, ctx, st, "default", "web")
			if p := placementAt(s, 0); p == nil || p.GetNodeId() != node || len(s.GetPlacements()) != 1 {
				t.Fatalf("placements = %v, want replica 0 still on %s alone", s.GetPlacements(), node)
			}
		})
	}
}

func TestLivenessFailedSingletonMovesOnlyToADiskReplica(t *testing.T) {
	ctx := context.Background()
	b := singletonBlock("vm")
	b.Spec.Placement = nil
	b.Spec.Storage = []*pb.Storage{{Name: "disk", Size: "1Gi"}}
	st := newStore(t)
	mustCreate(t, ctx, st, b)
	vname := storage.BlockVolumeName("default", "vm", "disk")
	nodes := nodeViews(3)
	nodes[0].HealthyVolumes = []string{vname}
	nodes[1].HealthyVolumes = []string{vname} // n3 holds no replica of the disk
	c := fixedNodes(st, nodes)
	reconcile(t, c)
	from := placementAt(waitPlaced(t, ctx, c, "default", "vm", 1), 0).GetNodeId()
	other := map[string]string{"n1": "n2", "n2": "n1"}[from]

	publishLiveness(t, st, "vm", 0, from, health.LivenessRecord{Failed: true})
	reconcile(t, c)
	reconcile(t, c)

	if p := placementAt(loadStatus(t, ctx, st, "default", "vm"), 0); p.GetNodeId() != other {
		t.Fatalf("singleton moved to %+v, want %s (the other disk replica)", p, other)
	}
}

func TestLivenessFailedDaemonsetReplicaIsMarkedNotMoved(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	mustCreate(t, ctx, st, daemonsetBlock("ds"))
	c := fixedNodes(st, nodeViews(2))
	reconcile(t, c)
	idx := placementOn(waitPlaced(t, ctx, c, "default", "ds", 2), "n1").GetReplicaIndex()

	publishLiveness(t, st, "ds", idx, "n1", health.LivenessRecord{Failed: true})
	reconcile(t, c)
	reconcile(t, c)

	s := loadStatus(t, ctx, st, "default", "ds")
	if p := placementOn(s, "n1"); p.GetPhase() != pb.Phase_FAILED || p.GetReplicaIndex() != idx {
		t.Fatalf("n1 placement = %+v, want FAILED in place", p)
	}
	if n := len(s.GetPlacements()); n != 2 || s.GetPhase() == pb.Phase_RUNNING {
		t.Fatalf("placements = %v, phase %v; want 2 and not RUNNING", s.GetPlacements(), s.GetPhase())
	}
}

func TestLivenessFailureOnTwoNodesStopsTheReplica(t *testing.T) {
	ctx := context.Background()
	b := blockFor("web", 1)
	b.Spec.Placement = nil
	st := newStore(t)
	mustCreate(t, ctx, st, b)
	c := fixedNodes(st, nodeViews(3))
	reconcile(t, c)
	first := placementAt(waitPlaced(t, ctx, c, "default", "web", 1), 0).GetNodeId()
	publishLiveness(t, st, "web", 0, first, health.LivenessRecord{Restarts: 1, Failed: true, Detail: "timeout"})
	reconcile(t, c)
	second := placementAt(loadStatus(t, ctx, st, "default", "web"), 0).GetNodeId()
	if m := failedPlacementOn(loadStatus(t, ctx, st, "default", "web"), first).GetMessage(); m != "liveness probe failed after 1 restart: timeout" {
		t.Errorf("message = %q", m)
	}

	publishLiveness(t, st, "web", 0, second, health.LivenessRecord{Failed: true})
	reconcile(t, c)
	reconcile(t, c)

	s := loadStatus(t, ctx, st, "default", "web")
	if p := placementAt(s, 0); p != nil {
		t.Fatalf("replica 0 placed on %s after failing on %s and %s", p.GetNodeId(), first, second)
	}
	for _, n := range []string{first, second} {
		if p := failedPlacementOn(s, n); p == nil || p.GetFormerIndex() != 0 {
			t.Fatalf("record on %s = %+v, want FAILED for former replica 0", n, p)
		}
	}
	if s.GetPhase() != pb.Phase_FAILED || s.GetPendingReason().GetCode() != "LivenessFailed" {
		t.Fatalf("block phase = %v, reason %+v; want FAILED, LivenessFailed", s.GetPhase(), s.GetPendingReason())
	}
}

func TestStoppedReplicaLeavesTheOthersRunningDegraded(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	mustCreate(t, ctx, st, blockFor("web", 2))
	c := fixedNodes(st, nodeViews(4))
	reconcile(t, c)
	first := placementAt(waitPlaced(t, ctx, c, "default", "web", 2), 1).GetNodeId()
	publishLiveness(t, st, "web", 1, first, health.LivenessRecord{Failed: true})
	reconcile(t, c)
	second := placementAt(loadStatus(t, ctx, st, "default", "web"), 1).GetNodeId()
	publishLiveness(t, st, "web", 1, second, health.LivenessRecord{Failed: true})
	reconcile(t, c)
	reconcile(t, c)

	s := loadStatus(t, ctx, st, "default", "web")
	if placementAt(s, 1) != nil || placementAt(s, 0) == nil {
		t.Fatalf("placements = %v, want replica 1 stopped and replica 0 kept", s.GetPlacements())
	}
	if p := failedPlacementOn(s, second); p.GetFormerIndex() != 1 {
		t.Fatalf("record on %s = %+v, want former replica 1", second, p)
	}
	if s.GetPhase() != pb.Phase_DEGRADED || s.GetPendingReason().GetCode() != "LivenessFailed" {
		t.Fatalf("block phase = %v, reason %+v; want DEGRADED, LivenessFailed", s.GetPhase(), s.GetPendingReason())
	}
}

func TestNewRevisionRestartsAStoppedReplica(t *testing.T) {
	ctx := context.Background()
	b := blockFor("web", 1)
	b.Spec.Placement = nil
	st := newStore(t)
	mustCreate(t, ctx, st, b)
	c := fixedNodes(st, nodeViews(2))
	reconcile(t, c)
	for range livenessNodesBeforeStop {
		node := placementAt(waitPlaced(t, ctx, c, "default", "web", len(loadStatus(t, ctx, st, "default", "web").GetPlacements())), 0).GetNodeId()
		publishLiveness(t, st, "web", 0, node, health.LivenessRecord{Failed: true})
		reconcile(t, c)
		if err := health.RetireLiveness(ctx, st, blockKey("default", "web"), 0, node); err != nil {
			t.Fatal(err) // the agent drops its record once the replica leaves its node
		}
	}
	if s := loadStatus(t, ctx, st, "default", "web"); s.GetPhase() != pb.Phase_FAILED {
		t.Fatalf("phase = %v, want FAILED before the fix", s.GetPhase())
	}

	b.Spec.Version = "2"
	mustCreate(t, ctx, st, b) // a new revision of the spec
	for range 5 {
		reconcile(t, c)
	}
	s := loadStatus(t, ctx, st, "default", "web")
	if p := placementAt(s, 0); p == nil || s.GetPhase() == pb.Phase_FAILED {
		t.Fatalf("after the fix: phase %v, placements %v; want replica 0 placed again", s.GetPhase(), s.GetPlacements())
	}
}
