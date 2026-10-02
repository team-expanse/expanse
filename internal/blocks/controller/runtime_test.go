package controller

// RuntimePass (T20.5b) tests: promotion of SCHEDULING placements to
// RUNNING based on the agent-published per-resource status records.
import (
	"context"
	"testing"

	"github.com/expanse/expanse/internal/scheduler"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
)

func runtimeHarness(t *testing.T) (*Controller, context.Context) {
	t.Helper()
	st := newStore(t)
	ctx := context.Background()
	c := New(st, func(context.Context) ([]scheduler.NodeView, scheduler.OvercommitConfig, error) {
		return nodeViews(2), testCfg(), nil
	})
	return c, ctx
}

func TestRuntimePassPromotes(t *testing.T) {
	c, ctx := runtimeHarness(t)
	mustCreate(t, ctx, c.St, blockFor("web", 2))
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	s := loadStatus(t, ctx, c.St, "default", "web")
	if len(s.GetPlacements()) != 2 {
		t.Fatalf("placements = %d, want 2", len(s.GetPlacements()))
	}
	if s.GetPhase() != pb.Phase_SCHEDULING {
		t.Fatalf("phase = %v, want SCHEDULING", s.GetPhase())
	}

	// Agent status records appear for replica 0 only.
	for _, p := range s.GetPlacements() {
		if p.GetReplicaIndex() != 0 {
			continue
		}
		key := "/node/" + p.GetNodeId() + "/status/resources/" +
			ReplicaResourceID("default", "web", 0)
		if _, err := c.St.Put(ctx, store.Key(key),
			[]byte("health=healthy in_sync=true actions=1 updated=1")); err != nil {
			t.Fatalf("put replica status: %v", err)
		}
	}

	if err := c.RuntimePass(ctx); err != nil {
		t.Fatalf("RuntimePass: %v", err)
	}
	s = loadStatus(t, ctx, c.St, "default", "web")
	phases := map[int32]pb.Phase{}
	for _, p := range s.GetPlacements() {
		phases[p.GetReplicaIndex()] = p.GetPhase()
	}
	if phases[0] != pb.Phase_RUNNING {
		t.Errorf("replica 0 phase = %v, want RUNNING", phases[0])
	}
	if phases[1] != pb.Phase_SCHEDULING {
		t.Errorf("replica 1 phase = %v, want SCHEDULING", phases[1])
	}
	// Block stays SCHEDULING until every replica is there.
	if s.GetPhase() != pb.Phase_SCHEDULING {
		t.Errorf("block phase = %v, want SCHEDULING (partial)", s.GetPhase())
	}

	// Second replica comes up → block promotes.
	s2 := loadStatus(t, ctx, c.St, "default", "web")
	for _, p := range s2.GetPlacements() {
		if p.GetReplicaIndex() != 1 {
			continue
		}
		key := "/node/" + p.GetNodeId() + "/status/resources/" +
			ReplicaResourceID("default", "web", 1)
		if _, err := c.St.Put(ctx, store.Key(key),
			[]byte("health=healthy in_sync=true actions=1 updated=1")); err != nil {
			t.Fatalf("put replica status: %v", err)
		}
	}
	if err := c.RuntimePass(ctx); err != nil {
		t.Fatalf("RuntimePass 2: %v", err)
	}
	s = loadStatus(t, ctx, c.St, "default", "web")
	if s.GetPhase() != pb.Phase_RUNNING {
		t.Errorf("block phase = %v, want RUNNING", s.GetPhase())
	}
}

// RuntimePass runs inside Reconcile (promotions happen on every pass).
func TestRuntimePassInReconcile(t *testing.T) {
	c, ctx := runtimeHarness(t)
	mustCreate(t, ctx, c.St, blockFor("web", 1))
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile 1: %v", err)
	}
	// No agent record yet: still SCHEDULING after another pass.
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile 2: %v", err)
	}
	if s := loadStatus(t, ctx, c.St, "default", "web"); s.GetPhase() != pb.Phase_SCHEDULING {
		t.Fatalf("phase = %v, want SCHEDULING", s.GetPhase())
	}
	s := loadStatus(t, ctx, c.St, "default", "web")
	p := s.GetPlacements()[0]
	key := "/node/" + p.GetNodeId() + "/status/resources/" +
		ReplicaResourceID("default", "web", 0)
	if _, err := c.St.Put(ctx, store.Key(key),
		[]byte("health=healthy in_sync=true actions=1 updated=1")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile 3: %v", err)
	}
	if s := loadStatus(t, ctx, c.St, "default", "web"); s.GetPhase() != pb.Phase_RUNNING {
		t.Fatalf("phase = %v, want RUNNING", s.GetPhase())
	}
}

// A healthy record that is not in sync does not promote.
func TestRuntimePassRequiresInSync(t *testing.T) {
	c, ctx := runtimeHarness(t)
	mustCreate(t, ctx, c.St, blockFor("web", 1))
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	s := loadStatus(t, ctx, c.St, "default", "web")
	p := s.GetPlacements()[0]
	key := "/node/" + p.GetNodeId() + "/status/resources/" +
		ReplicaResourceID("default", "web", 0)
	if _, err := c.St.Put(ctx, store.Key(key),
		[]byte(`health=healthy in_sync=false actions=3 updated=1`)); err != nil {
		t.Fatal(err)
	}
	if err := c.RuntimePass(ctx); err != nil {
		t.Fatalf("RuntimePass: %v", err)
	}
	if s := loadStatus(t, ctx, c.St, "default", "web"); s.GetPhase() != pb.Phase_SCHEDULING {
		t.Fatalf("phase = %v, want SCHEDULING (not in sync)", s.GetPhase())
	}
}

// Removed agent records never demote a RUNNING placement back.
func TestRuntimePassNeverDemotes(t *testing.T) {
	c, ctx := runtimeHarness(t)
	mustCreate(t, ctx, c.St, blockFor("web", 1))
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	s := loadStatus(t, ctx, c.St, "default", "web")
	p := s.GetPlacements()[0]
	key := "/node/" + p.GetNodeId() + "/status/resources/" +
		ReplicaResourceID("default", "web", 0)
	if _, err := c.St.Put(ctx, store.Key(key),
		[]byte(`health=healthy in_sync=true actions=1 updated=1`)); err != nil {
		t.Fatal(err)
	}
	if err := c.RuntimePass(ctx); err != nil {
		t.Fatalf("RuntimePass: %v", err)
	}
	// Record disappears (agent blip): phase must stay RUNNING.
	if err := c.St.Delete(ctx, store.Key(key), 0); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := c.RuntimePass(ctx); err != nil {
		t.Fatalf("RuntimePass 2: %v", err)
	}
	if s := loadStatus(t, ctx, c.St, "default", "web"); s.GetPhase() != pb.Phase_RUNNING {
		t.Fatalf("phase = %v, want RUNNING (no demotion)", s.GetPhase())
	}
}

// Retired (LOST) records and daemonset placements are untouched by
// promotion; ensure the pass doesn't corrupt unrelated records.
func TestRuntimePassSkipsRetired(t *testing.T) {
	c, ctx := runtimeHarness(t)
	mustCreate(t, ctx, c.St, blockFor("web", 1))
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	s := loadStatus(t, ctx, c.St, "default", "web")
	s.Placements = append(s.Placements, &pb.PlacementStatus{
		ReplicaIndex: -1, NodeId: "n1", Phase: pb.Phase_LOST,
	})
	out, err := proto.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.St.Txn(ctx, []store.Op{
		{Kind: store.OpPut, Key: statusKey(blockKey("default", "web")), Value: out},
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.RuntimePass(ctx); err != nil {
		t.Fatalf("RuntimePass: %v", err)
	}
	if s := loadStatus(t, ctx, c.St, "default", "web"); s == nil {
		t.Fatal("status vanished")
	}
}

// A replica whose workload reports not ready (a booting VM guest) is not promoted until it is.
func TestRuntimePassWaitsForReadiness(t *testing.T) {
	c, ctx := runtimeHarness(t)
	mustCreate(t, ctx, c.St, blockFor("web", 1))
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	p := loadStatus(t, ctx, c.St, "default", "web").GetPlacements()[0]
	key := store.Key("/node/" + p.GetNodeId() + "/status/resources/" + ReplicaResourceID("default", "web", 0))
	put := func(v string) {
		t.Helper()
		if _, err := c.St.Put(ctx, key, []byte(v)); err != nil {
			t.Fatal(err)
		}
		if err := c.RuntimePass(ctx); err != nil {
			t.Fatalf("RuntimePass: %v", err)
		}
	}

	put(`health=healthy in_sync=true ready=false actions=0 error="saw ready=true" updated=1`)
	if s := loadStatus(t, ctx, c.St, "default", "web"); s.GetPhase() != pb.Phase_SCHEDULING {
		t.Fatalf("phase = %v, want SCHEDULING while not ready", s.GetPhase())
	}
	put(`health=healthy in_sync=true ready=true actions=0 error="" updated=2`)
	if s := loadStatus(t, ctx, c.St, "default", "web"); s.GetPhase() != pb.Phase_RUNNING {
		t.Fatalf("phase = %v, want RUNNING once ready", s.GetPhase())
	}
}

// A replica with a readiness probe is RUNNING only once its own node publishes a passing result.
func TestRuntimePassWaitsForReadinessProbe(t *testing.T) {
	c, ctx := runtimeHarness(t)
	b := blockFor("web", 1)
	b.Spec.Network = &pb.Network{HealthCheck: &pb.HealthCheck{
		Readiness: &pb.HealthProbe{Type: pb.ProbeType_PROBE_TCP, Port: 80},
	}}
	mustCreate(t, ctx, c.St, b)
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	node := loadStatus(t, ctx, c.St, "default", "web").GetPlacements()[0].GetNodeId()
	unit := store.Key("/node/" + node + "/status/resources/" + ReplicaResourceID("default", "web", 0))
	if _, err := c.St.Put(ctx, unit, []byte("health=healthy in_sync=true actions=1 updated=1")); err != nil {
		t.Fatal(err)
	}
	probe := store.Key("/blocks/default/web/status/replicas/0")
	for _, step := range []struct {
		record string // "" leaves no record
		want   pb.Phase
	}{
		{"", pb.Phase_SCHEDULING},
		{`{"ok":false,"node":"` + node + `"}`, pb.Phase_SCHEDULING},
		{`{"ok":true,"node":"another-node"}`, pb.Phase_SCHEDULING},
		{`{"ok":true,"node":"` + node + `"}`, pb.Phase_RUNNING},
	} {
		if step.record != "" {
			if _, err := c.St.Put(ctx, probe, []byte(step.record)); err != nil {
				t.Fatal(err)
			}
		}
		if err := c.RuntimePass(ctx); err != nil {
			t.Fatalf("RuntimePass: %v", err)
		}
		if got := loadStatus(t, ctx, c.St, "default", "web").GetPhase(); got != step.want {
			t.Fatalf("record %q: phase = %v, want %v", step.record, got, step.want)
		}
	}
}
