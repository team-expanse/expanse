package controller

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/boltstore"
)

func newStore(t *testing.T) *boltstore.Store {
	t.Helper()
	st, err := boltstore.New(filepath.Join(t.TempDir(), "ctl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func seedVolume(t *testing.T, ctx context.Context, st *boltstore.Store, id string, repl int, nodes []string, primary string, state storage.VolumeState) storage.Status {
	t.Helper()
	spec := storage.Spec{ID: id, Name: id, Replication: repl, SizeBytes: 1 << 30}
	if err := storage.SaveSpec(ctx, st, spec); err != nil {
		t.Fatal(err)
	}
	status := storage.Status{State: state, Primary: primary}
	for i, n := range nodes {
		role := storage.RoleSecondary
		if n == primary {
			role = storage.RolePrimary
		} else if i == 0 && primary == "" {
			role = storage.RolePrimary
			primary = n
		}
		status.Placement = append(status.Placement, storage.Replica{
			NodeID: n, Role: role, ZvolPath: "pool/volumes/" + id,
			Healthy: true, Sequence: uint64(10 - i),
		})
	}
	if err := storage.SaveStatus(ctx, st, id, status); err != nil {
		t.Fatal(err)
	}
	return status
}

func seedMesh(st *boltstore.Store, nodes ...string) {
	for _, n := range nodes {
		_, _ = st.Put(context.Background(),
			store.Key("/nodes/"+n+"/network.wgPublicKey"), []byte("k"))
	}
}

// TestCreateToElectedPrimaries: creation through the runtime's
// leader-gated placement, then the controller's election rule — a lost
// primary is re-elected by HIGHEST seq, ties by lowest node ID, and
// the volume is marked Degraded until recovery levels it.
func TestCreateToElectedPrimaries(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedMesh(st, "n2", "n3") // n1 permanently gone from the mesh
	// n2 has seq 12 (highest), n3 has 11.
	seedVolume(t, ctx, st, "vol-aaa", 3, []string{"n1", "n2", "n3"}, "n1", storage.StateHealthy)
	status, _, _ := storage.LoadStatus(ctx, st, "vol-aaa")
	status.Placement[1].Sequence = 12
	status.Placement[2].Sequence = 11
	_ = storage.SaveStatus(ctx, st, "vol-aaa", status)

	var leader atomic.Bool
	leader.Store(true)
	c := New(Options{St: st, Pool: "pool", IsLeader: leader.Load, Alert: func(AlertEvent) {}})
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	got, _, err := storage.LoadStatus(ctx, st, "vol-aaa")
	if err != nil {
		t.Fatal(err)
	}
	if got.Primary != "n2" {
		t.Fatalf("elected %q, want n2 (highest seq 12)", got.Primary)
	}
	if got.State != storage.StateDegraded {
		t.Fatalf("state = %s, want Degraded until recovery", got.State)
	}
}

// TestElectionTieLowestNodeID.
func TestElectionTieLowestNodeID(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedMesh(st, "n1", "n2", "n3")
	seedVolume(t, ctx, st, "vol-tie", 3, []string{"n1", "n2", "n3"}, "n1", storage.StateHealthy)
	status, _, _ := storage.LoadStatus(ctx, st, "vol-tie")
	for i := range status.Placement { // all three at seq 10 → lowest ID wins
		status.Placement[i].Sequence = 10
	}
	_ = storage.SaveStatus(ctx, st, "vol-tie", status)

	c := New(Options{St: st, Pool: "pool", IsLeader: func() bool { return true }})
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	got, _, _ := storage.LoadStatus(ctx, st, "vol-tie")
	if got.Primary != "n1" {
		t.Fatalf("elected %q, want n1 (tie → lowest ID)", got.Primary)
	}
}

// TestNodeLossRebuild: a permanently-lost node's replica is replaced
// with a Resyncing row on a meshed node; scheduling is bounded to 2 in
// flight per node (a third pass does not stack a third rebuild).
func TestNodeLossRebuild(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedMesh(st, "n1", "n2", "n3")
	// Volume placed on n1 (primary), n2, n9 — n9 is permanently lost
	// (not meshed).
	seedVolume(t, ctx, st, "vol-lost", 3, []string{"n1", "n2", "n9"}, "n1", storage.StateHealthy)

	c := New(Options{St: st, Pool: "pool", IsLeader: func() bool { return true }})
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	got, _, _ := storage.LoadStatus(ctx, st, "vol-lost")
	var rebuildNode string
	for _, p := range got.Placement {
		if p.Role == storage.RoleResyncing {
			rebuildNode = p.NodeID
			if p.Healthy {
				t.Fatal("rebuild replica must start unhealthy")
			}
			if p.NodeID == "n9" {
				t.Fatal("lost node must be replaced")
			}
		}
	}
	if rebuildNode == "" {
		t.Fatal("no rebuild scheduled for the lost replica")
	}
	// Bounded concurrency: 2 rebuilds max per node — the same node
	// takes more volumes' rebuilds only up to the cap.
	if c.rebuilds[rebuildNode] != 1 {
		t.Fatalf("rebuild count = %d, want 1", c.rebuilds[rebuildNode])
	}
	c.RebuildDone(rebuildNode)
	if c.rebuilds[rebuildNode] != 0 {
		t.Fatal("RebuildDone must release the slot")
	}
}

// TestRebuildCapTwo: three rebuilds onto the same node → only 2
// scheduled.
func TestRebuildCapTwo(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedMesh(st, "n1", "n2")
	for _, id := range []string{"vol-a", "vol-b", "vol-c"} {
		seedVolume(t, ctx, st, id, 2, []string{"n1", "n9"}, "n1", storage.StateHealthy)
	}
	c := New(Options{St: st, Pool: "pool", IsLeader: func() bool { return true }})
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if c.rebuilds["n2"] > 2 {
		t.Fatalf("rebuilds on n2 = %d, cap is 2", c.rebuilds["n2"])
	}
}

// TestUnderReplicationAlert: fewer healthy replicas than the factor →
// alert event emitted and state Degraded.
func TestUnderReplicationAlert(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedMesh(st, "n1", "n2", "n3")
	seedVolume(t, ctx, st, "vol-under", 3, []string{"n1", "n2", "n3"}, "n1", storage.StateHealthy)
	status, rev, _ := storage.LoadStatus(ctx, st, "vol-under")
	status.Placement[1].Healthy = false
	status.Placement[2].Healthy = false
	_ = storage.SaveStatus(ctx, st, "vol-under", status)
	_ = rev

	var alerts []AlertEvent
	c := New(Options{
		St: st, Pool: "pool", IsLeader: func() bool { return true },
		Alert: func(ev AlertEvent) { alerts = append(alerts, ev) },
	})
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if len(alerts) == 0 || alerts[0].Kind != "under-replication" || alerts[0].Have != 1 || alerts[0].Want != 3 {
		t.Fatalf("alerts = %+v, want under-replication 1/3", alerts)
	}
	got, _, _ := storage.LoadStatus(ctx, st, "vol-under")
	if got.State != storage.StateDegraded {
		t.Fatalf("state = %s, want Degraded", got.State)
	}
}

// TestDeleteEndToEnd: Delete marks Deleting; the next reconcile
// destroys zvols and drops both records.
func TestDeleteEndToEnd(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedMesh(st, "n1", "n2", "n3")
	seedVolume(t, ctx, st, "vol-del", 3, []string{"n1", "n2", "n3"}, "n1", storage.StateHealthy)
	destroyed := map[string]bool{}
	c := New(Options{
		St: st, Pool: "pool", IsLeader: func() bool { return true },
		ZFS: fakeCtlZFS{destroyed: destroyed},
	})
	if err := c.Delete(ctx, "vol-del"); err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if !destroyed["pool/volumes/vol-del"] {
		t.Fatalf("zvols not destroyed: %v", destroyed)
	}
	if _, _, err := storage.LoadStatus(ctx, st, "vol-del"); err == nil {
		t.Fatal("status record should be gone")
	}
	if _, err := storage.LoadSpec(ctx, st, "vol-del"); err == nil {
		t.Fatal("spec record should be gone")
	}
}

type fakeCtlZFS struct{ destroyed map[string]bool }

func (f fakeCtlZFS) DestroyZvol(_ context.Context, zvol string, _ bool) error {
	f.destroyed[zvol] = true
	return nil
}
func (f fakeCtlZFS) Scrub(_ context.Context, _ string) error { return nil }

// TestScrubMonthly: the scrub runs once per interval, not per tick.
func TestScrubMonthly(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	var scrubs atomic.Int32
	c := New(Options{
		St: st, Pool: "pool", IsLeader: func() bool { return true },
		ScrubInterval: time.Hour,
		ZFS:           scrubZFS{fn: func() { scrubs.Add(1) }},
	})
	for i := 0; i < 5; i++ {
		if err := c.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if scrubs.Load() != 1 {
		t.Fatalf("scrubs = %d, want 1 per interval", scrubs.Load())
	}
}

type scrubZFS struct{ fn func() }

func (s scrubZFS) DestroyZvol(context.Context, string, bool) error { return nil }
func (s scrubZFS) Scrub(context.Context, string) error {
	s.fn()
	return nil
}

// TestNeedsManualRecoveryUntouched: §9 — the controller never touches
// a diverged volume.
func TestNeedsManualRecoveryUntouched(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedMesh(st, "n1", "n2", "n3")
	seedVolume(t, ctx, st, "vol-div", 3, []string{"n1", "n2", "n3"}, "n1", storage.StateNeedsManualRecovery)
	c := New(Options{St: st, Pool: "pool", IsLeader: func() bool { return true }})
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	got, _, _ := storage.LoadStatus(ctx, st, "vol-div")
	if got.State != storage.StateNeedsManualRecovery || got.Primary != "n1" {
		t.Fatalf("controller modified a diverged volume: %+v", got)
	}
}

// TestNonLeaderInert: a non-leader controller never mutates state.
func TestNonLeaderInert(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedMesh(st, "n1", "n2")
	seedVolume(t, ctx, st, "vol-x", 2, []string{"n1", "n2"}, "", storage.StateHealthy)
	c := New(Options{St: st, Pool: "pool", IsLeader: func() bool { return false }})
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	got, _, _ := storage.LoadStatus(ctx, st, "vol-x")
	if got.Primary != "" {
		t.Fatalf("non-leader elected a primary: %+v", got)
	}
}

// TestOpsDeleteAndMovePrimary (T15 §4.8): the op records the ctl writes
// are consumed by the leader's controller — delete by volume NAME, and
// move-primary only to a node that already holds a replica.
func TestOpsDeleteAndMovePrimary(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedMesh(st, "n1", "n2", "n3")
	seedVolume(t, ctx, st, "vol-op", 3, []string{"n1", "n2", "n3"}, "n1", storage.StateHealthy)
	destroyed := map[string]bool{}
	c := New(Options{
		St: st, Pool: "pool", IsLeader: func() bool { return true },
		ZFS: fakeCtlZFS{destroyed: destroyed},
	})

	// move-primary: legal target (n2 holds a replica).
	op := []byte(`{"target":"vol-op","to":"n2"}`)
	if _, err := st.Put(ctx, store.Key("/volumes/_ops/move-primary/vol-op"), op); err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	status, _, err := storage.LoadStatus(ctx, st, "vol-op")
	if err != nil {
		t.Fatal(err)
	}
	if status.Primary != "n2" {
		t.Fatalf("primary = %s, want n2", status.Primary)
	}
	if _, err := st.Get(ctx, store.Key("/volumes/_ops/move-primary/vol-op")); err == nil {
		t.Fatal("move-primary op not consumed")
	}

	// move-primary refused: n9 holds no replica.
	if _, err := st.Put(ctx, store.Key("/volumes/_ops/move-primary/vol-op"), []byte(`{"target":"vol-op","to":"n9"}`)); err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if status, _, _ = storage.LoadStatus(ctx, st, "vol-op"); status.Primary != "n2" {
		t.Fatalf("primary moved to non-replica node: %s", status.Primary)
	}

	// delete by NAME → volume gone + zvol destroyed.
	if _, err := st.Put(ctx, store.Key("/volumes/_ops/delete/vol-op"), []byte(`{"target":"vol-op"}`)); err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := storage.LoadStatus(ctx, st, "vol-op"); err == nil {
		t.Fatal("volume should be deleted via op record")
	}
	if !destroyed["pool/volumes/vol-op"] {
		t.Fatal("zvol not destroyed via delete op")
	}
}
