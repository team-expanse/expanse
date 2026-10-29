package controller

import (
	"context"
	"testing"

	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/raftstore"
	pb "github.com/expanse/expanse/proto"
)

// report writes the node's status record for replica 0 of vm, as its reconciler would.
func report(t *testing.T, ctx context.Context, st *raftstore.Store, node, record string) {
	t.Helper()
	key := "/node/" + node + "/status/resources/" + ReplicaResourceID("default", "vm", 0)
	if _, err := st.Put(ctx, store.Key(key), []byte(record)); err != nil {
		t.Fatal(err)
	}
}

func passes(t *testing.T, ctx context.Context, c *Controller, n int) *pb.BlockStatus {
	t.Helper()
	for range n {
		if _, err := c.Reconcile(ctx); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}
	return loadStatus(t, ctx, c.St, "default", "vm")
}

// runningVM places a singleton vm and brings replica 0 to RUNNING; it returns the replica's node.
func runningVM(t *testing.T, ctx context.Context, c *Controller) (*pb.Block, string) {
	t.Helper()
	b := singletonBlock("vm")
	mustCreate(t, ctx, c.St, b)
	s := passes(t, ctx, c, 1)
	node := s.GetPlacements()[0].GetNodeId()
	report(t, ctx, c.St, node, `health=healthy in_sync=true ready=true actions=0 error="" updated=1 status="ready: up"`)
	if s = passes(t, ctx, c, 2); s.GetPhase() != pb.Phase_RUNNING {
		t.Fatalf("phase = %v, want RUNNING", s.GetPhase())
	}
	return b, node
}

func TestASuspendedReplicaIsRecordedWithItsSave(t *testing.T) {
	c, ctx := runtimeHarness(t)
	b, node := runningVM(t, ctx, c)

	b.Spec.Power = pb.Power_POWER_SUSPENDED
	mustCreate(t, ctx, c.St, b)
	s := passes(t, ctx, c, 2)
	if s.GetPhase() == pb.Phase_UPDATING || s.GetPhase() == pb.Phase_SUSPENDED {
		t.Fatalf("before the node stops it: phase = %v", s.GetPhase())
	}
	report(t, ctx, c.St, node, `health=healthy in_sync=true actions=1 error="" updated=2 status="suspended r9"`)
	s = passes(t, ctx, c, 1)
	p := s.GetPlacements()[0]
	if s.GetPhase() != pb.Phase_SUSPENDED || p.GetPhase() != pb.Phase_SUSPENDED || p.GetSavedState() != "r9" {
		t.Fatalf("block %v, replica %v saved %q; want SUSPENDED, SUSPENDED, r9", s.GetPhase(), p.GetPhase(), p.GetSavedState())
	}
	if p.GetNodeId() != node {
		t.Errorf("the suspended replica moved to %s", p.GetNodeId())
	}
	if s = passes(t, ctx, c, 2); s.GetPhase() != pb.Phase_SUSPENDED {
		t.Errorf("phase later = %v, want SUSPENDED", s.GetPhase())
	}
}

func TestAResumedReplicaRunsOnceItsNodeStartsIt(t *testing.T) {
	c, ctx := runtimeHarness(t)
	b, node := runningVM(t, ctx, c)
	b.Spec.Power = pb.Power_POWER_SUSPENDED
	mustCreate(t, ctx, c.St, b)
	report(t, ctx, c.St, node, `health=healthy in_sync=true actions=1 error="" updated=2 status="suspended r9"`)
	passes(t, ctx, c, 2)

	b.Spec.Power = pb.Power_POWER_ON
	mustCreate(t, ctx, c.St, b)
	s := passes(t, ctx, c, 2)
	p := s.GetPlacements()[0]
	if p.GetPhase() == pb.Phase_RUNNING || p.GetPhase() == pb.Phase_SUSPENDED || s.GetPhase() == pb.Phase_SUSPENDED {
		t.Fatalf("on the node's stale report: block %v replica %v, want neither running nor suspended", s.GetPhase(), p.GetPhase())
	}
	if p.GetSavedState() != "r9" || p.GetNodeId() != node {
		t.Errorf("resume lost the save: %q on %s", p.GetSavedState(), p.GetNodeId())
	}
	report(t, ctx, c.St, node, `health=healthy in_sync=true ready=true actions=1 error="" updated=3 status="ready: resumed"`)
	if s = passes(t, ctx, c, 2); s.GetPhase() != pb.Phase_RUNNING || s.GetPlacements()[0].GetPhase() != pb.Phase_RUNNING {
		t.Errorf("after the node resumed it: block %v replica %v, want RUNNING", s.GetPhase(), s.GetPlacements()[0].GetPhase())
	}
}

func TestAReplicaStoppedWithoutASaveIsSuspendedWithNone(t *testing.T) {
	c, ctx := runtimeHarness(t)
	b, node := runningVM(t, ctx, c)
	b.Spec.Power = pb.Power_POWER_SUSPENDED
	mustCreate(t, ctx, c.St, b)
	report(t, ctx, c.St, node, `health=healthy in_sync=true actions=1 error="" updated=2 status="stopped"`)
	s := passes(t, ctx, c, 1)
	if p := s.GetPlacements()[0]; s.GetPhase() != pb.Phase_SUSPENDED || p.GetSavedState() != "" {
		t.Errorf("block %v saved %q, want SUSPENDED with nothing saved", s.GetPhase(), p.GetSavedState())
	}
}
