package controller

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/storage/drbd"
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

// seedVolume stores a volume whose replicas all report current data.
func seedVolume(t *testing.T, st *boltstore.Store, id string, repl int, nodes []string, primary string, state storage.VolumeState) {
	t.Helper()
	ctx := context.Background()
	if err := storage.SaveSpec(ctx, st, storage.Spec{ID: id, Name: id, Replication: repl, SizeBytes: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	status := storage.Status{State: state, Primary: primary}
	for _, n := range nodes {
		role := storage.RoleSecondary
		if n == primary {
			role = storage.RolePrimary
		}
		status.Placement = append(status.Placement, storage.Replica{NodeID: n, Role: role, Healthy: true})
	}
	if err := storage.SaveStatus(ctx, st, id, status); err != nil {
		t.Fatal(err)
	}
}

func seedMesh(st *boltstore.Store, nodes ...string) {
	for _, n := range nodes {
		_, _ = st.Put(context.Background(), store.Key("/nodes/"+n+"/network.wgPublicKey"), []byte("k"))
	}
}

// edit rewrites a volume's status in place.
func edit(t *testing.T, st *boltstore.Store, id string, fn func(*storage.Status)) {
	t.Helper()
	ctx := context.Background()
	status, rev, err := storage.LoadStatus(ctx, st, id)
	if err != nil {
		t.Fatal(err)
	}
	fn(&status)
	if err := storage.CompareAndSwapStatus(ctx, st, id, rev, status); err != nil {
		t.Fatal(err)
	}
}

func leaderCtl(st *boltstore.Store, alert func(AlertEvent)) *Controller {
	return New(Options{St: st, IsLeader: func() bool { return true }, Alert: alert, Alloc: drbd.NewAllocator(st, drbd.DefaultMinors, drbd.DefaultPorts)})
}

func reconcile(t *testing.T, c *Controller) {
	t.Helper()
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func load(t *testing.T, st *boltstore.Store, id string) storage.Status {
	t.Helper()
	s, _, err := storage.LoadStatus(context.Background(), st, id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestDeadPrimaryIsReplacedByTheLowestHealthySecondary(t *testing.T) {
	st := newStore(t)
	seedMesh(st, "n2", "n3") // n1 is gone from the mesh
	seedVolume(t, st, "vol-a", 3, []string{"n1", "n2", "n3"}, "n1", storage.StateHealthy)
	reconcile(t, leaderCtl(st, func(AlertEvent) {}))
	got := load(t, st, "vol-a")
	if got.Primary != "n2" {
		t.Fatalf("elected %q, want n2", got.Primary)
	}
	if got.State != storage.StateDegraded {
		t.Errorf("state = %s, want Degraded with 2 of 3 healthy", got.State)
	}
}

func TestPrimaryWhoseNodeLivenessLeaseExpiredIsReplaced(t *testing.T) {
	st := newStore(t)
	seedMesh(st, "n1", "n2", "n3") // a hard-killed node's mesh record outlives it
	seedVolume(t, st, "vol-a", 3, []string{"n1", "n2", "n3"}, "n1", storage.StateHealthy)
	if _, err := st.Put(context.Background(), "/leases/node-n1", []byte(`{"h":"n1","e":-1}`)); err != nil {
		t.Fatal(err)
	}
	reconcile(t, leaderCtl(st, func(AlertEvent) {}))
	if got := load(t, st, "vol-a").Primary; got != "n2" {
		t.Fatalf("elected %q, want n2", got)
	}
}

func TestElectionSkipsReplicasThatAreBehind(t *testing.T) {
	st := newStore(t)
	seedMesh(st, "n2", "n3")
	seedVolume(t, st, "vol-a", 3, []string{"n1", "n2", "n3"}, "n1", storage.StateHealthy)
	edit(t, st, "vol-a", func(s *storage.Status) {
		s.Placement[1].Role, s.Placement[1].Healthy = storage.RoleStale, false
	})
	reconcile(t, leaderCtl(st, func(AlertEvent) {}))
	if got := load(t, st, "vol-a").Primary; got != "n3" {
		t.Fatalf("elected %q, want n3 (n2 is Stale)", got)
	}
}

func TestNoElectionWhenNoReplicaIsCurrent(t *testing.T) {
	st := newStore(t)
	seedMesh(st, "n2", "n3")
	seedVolume(t, st, "vol-a", 3, []string{"n1", "n2", "n3"}, "n1", storage.StateHealthy)
	edit(t, st, "vol-a", func(s *storage.Status) {
		s.Placement[1].Role, s.Placement[1].Healthy = storage.RoleStale, false
		s.Placement[2].Role, s.Placement[2].Healthy = storage.RoleResyncing, false
	})
	reconcile(t, leaderCtl(st, func(AlertEvent) {}))
	if got := load(t, st, "vol-a").Primary; got != "n1" {
		t.Fatalf("primary moved to %q although no replica has current data", got)
	}
}

func TestElectionTieGoesToTheLowestNodeID(t *testing.T) {
	st := newStore(t)
	seedMesh(st, "n1", "n2", "n3")
	seedVolume(t, st, "vol-a", 3, []string{"n3", "n2", "n1"}, "", storage.StateHealthy)
	reconcile(t, leaderCtl(st, func(AlertEvent) {}))
	if got := load(t, st, "vol-a").Primary; got != "n1" {
		t.Fatalf("elected %q, want n1", got)
	}
}

func TestHealthyPrimaryIsLeftAloneWithoutWritingTheStore(t *testing.T) {
	st := newStore(t)
	seedMesh(st, "n1", "n2", "n3")
	seedVolume(t, st, "vol-a", 3, []string{"n1", "n2", "n3"}, "n2", storage.StateHealthy)
	_, before, _ := storage.LoadStatus(context.Background(), st, "vol-a")
	reconcile(t, leaderCtl(st, func(AlertEvent) {}))
	if _, after, _ := storage.LoadStatus(context.Background(), st, "vol-a"); after != before {
		t.Fatalf("revision %d -> %d: rewrote a status with nothing to change", before, after)
	}
}

func TestPrimaryThatLostQuorumYieldsToAHealthyReplica(t *testing.T) {
	st := newStore(t)
	seedMesh(st, "n1", "n2", "n3")
	seedVolume(t, st, "vol-a", 3, []string{"n1", "n2", "n3"}, "n1", storage.StateHealthy)
	edit(t, st, "vol-a", func(s *storage.Status) { s.Placement[0].Healthy = false })
	reconcile(t, leaderCtl(st, func(AlertEvent) {}))
	if got := load(t, st, "vol-a").Primary; got != "n2" {
		t.Fatalf("elected %q, want n2", got)
	}
}

func TestPrimaryThatLostQuorumIsKeptWhenNobodyIsBetter(t *testing.T) {
	st := newStore(t)
	seedMesh(st, "n1", "n2", "n3")
	seedVolume(t, st, "vol-a", 3, []string{"n1", "n2", "n3"}, "n1", storage.StateHealthy)
	edit(t, st, "vol-a", func(s *storage.Status) {
		for i := range s.Placement {
			s.Placement[i].Healthy = false
		}
	})
	reconcile(t, leaderCtl(st, func(AlertEvent) {}))
	if got := load(t, st, "vol-a").Primary; got != "n1" {
		t.Fatalf("primary = %q, want n1", got)
	}
}

func TestCreatingVolumeKeepsItsPrimaryAndLeavesCreatingOnlyWhenEveryReplicaIsHealthy(t *testing.T) {
	st := newStore(t)
	seedMesh(st, "n1", "n2", "n3")
	seedVolume(t, st, "vol-a", 3, []string{"n1", "n2", "n3"}, "n1", storage.StateCreating)
	edit(t, st, "vol-a", func(s *storage.Status) {
		for i := range s.Placement {
			s.Placement[i].Role, s.Placement[i].Healthy = "", false
		}
	})
	alerts := 0
	c := leaderCtl(st, func(AlertEvent) { alerts++ })
	reconcile(t, c)
	if got := load(t, st, "vol-a"); got.Primary != "n1" || got.State != storage.StateCreating || alerts != 0 {
		t.Fatalf("status = %+v, alerts = %d", got, alerts)
	}
	edit(t, st, "vol-a", func(s *storage.Status) {
		for i := range s.Placement {
			s.Placement[i].Role, s.Placement[i].Healthy = storage.RoleSecondary, i != 2
		}
	})
	reconcile(t, c)
	if got := load(t, st, "vol-a").State; got != storage.StateCreating {
		t.Fatalf("state = %s with a replica still syncing", got)
	}
	edit(t, st, "vol-a", func(s *storage.Status) { s.Placement[2].Healthy = true })
	reconcile(t, c)
	if got := load(t, st, "vol-a").State; got != storage.StateHealthy {
		t.Fatalf("state = %s, want Healthy", got)
	}
}

func TestUnderReplicationAlert(t *testing.T) {
	st := newStore(t)
	seedMesh(st, "n1", "n2", "n3")
	seedVolume(t, st, "vol-a", 3, []string{"n1", "n2", "n3"}, "n1", storage.StateHealthy)
	edit(t, st, "vol-a", func(s *storage.Status) { s.Placement[2].Healthy = false })
	var alerts []AlertEvent
	reconcile(t, leaderCtl(st, func(ev AlertEvent) { alerts = append(alerts, ev) }))
	if len(alerts) == 0 || alerts[0].Kind != "under-replication" || alerts[0].Have != 2 || alerts[0].Want != 3 {
		t.Fatalf("alerts = %+v, want under-replication 2/3", alerts)
	}
	if got := load(t, st, "vol-a").State; got != storage.StateDegraded {
		t.Fatalf("state = %s, want Degraded", got)
	}
}

func TestStateFollowsTheHealthyCountBothWays(t *testing.T) {
	st := newStore(t)
	seedMesh(st, "n1", "n2", "n3")
	seedVolume(t, st, "vol-a", 3, []string{"n1", "n2", "n3"}, "n1", storage.StateHealthy)
	c := leaderCtl(st, func(AlertEvent) {})
	edit(t, st, "vol-a", func(s *storage.Status) { s.Placement[1].Healthy, s.Placement[2].Healthy = false, false })
	reconcile(t, c)
	if got := load(t, st, "vol-a").State; got != storage.StateReadOnly {
		t.Fatalf("state = %s, want ReadOnly below quorum", got)
	}
	edit(t, st, "vol-a", func(s *storage.Status) { s.Placement[1].Healthy, s.Placement[2].Healthy = true, true })
	reconcile(t, c)
	if got := load(t, st, "vol-a").State; got != storage.StateHealthy {
		t.Fatalf("state after recovery = %s, want Healthy", got)
	}
}

func TestDerivedStateByReplicationFactor(t *testing.T) {
	for _, tc := range []struct {
		healthy, replication int
		want                 storage.VolumeState
	}{
		{1, 1, storage.StateHealthy},
		{0, 1, storage.StateReadOnly},
		{2, 2, storage.StateHealthy},
		{1, 2, storage.StateDegraded}, // quorum is off at two replicas: a survivor keeps writing
		{0, 2, storage.StateReadOnly},
		{3, 3, storage.StateHealthy},
		{2, 3, storage.StateDegraded},
		{1, 3, storage.StateReadOnly},
		{3, 5, storage.StateDegraded},
		{2, 5, storage.StateReadOnly},
	} {
		if got := derivedState(tc.healthy, tc.replication); got != tc.want {
			t.Errorf("derivedState(%d of %d) = %s, want %s", tc.healthy, tc.replication, got, tc.want)
		}
	}
}

func TestDeleteWaitsForEveryNodeThenReleasesTheAllocation(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedMesh(st, "n1", "n2")
	c := leaderCtl(st, func(AlertEvent) {})
	request(t, st, "data", 2)
	reconcile(t, c)
	id := placed(t, st)[0]
	if err := c.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	reconcile(t, c)
	if _, err := storage.LoadSpec(ctx, st, id); err != nil {
		t.Fatal("records dropped while replicas still exist")
	}
	edit(t, st, id, func(s *storage.Status) { s.Placement = s.Placement[1:] })
	reconcile(t, c)
	if _, err := storage.LoadSpec(ctx, st, id); err != nil {
		t.Fatal("records dropped while one replica still exists")
	}
	edit(t, st, id, func(s *storage.Status) { s.Placement = nil })
	reconcile(t, c)
	if _, _, err := storage.LoadStatus(ctx, st, id); err == nil {
		t.Fatal("status record should be gone")
	}
	if _, err := storage.LoadSpec(ctx, st, id); err == nil {
		t.Fatal("spec record should be gone")
	}
	if _, err := c.opts.Alloc.Get(ctx, id); err == nil {
		t.Fatal("allocation not released: the minor and port would leak")
	}
}

func TestDeleteFinishesEvenIfTheAllocationWasAlreadyReleased(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedMesh(st, "n1")
	seedVolume(t, st, "vol-a", 1, nil, "", storage.StateDeleting)
	reconcile(t, leaderCtl(st, func(AlertEvent) {}))
	if _, err := storage.LoadSpec(ctx, st, "vol-a"); err == nil {
		t.Fatal("a crash after Release must not wedge the delete")
	}
}

func TestNeedsManualRecoveryIsUntouched(t *testing.T) {
	st := newStore(t)
	seedMesh(st, "n2", "n3")
	seedVolume(t, st, "vol-a", 3, []string{"n1", "n2", "n3"}, "n1", storage.StateNeedsManualRecovery)
	reconcile(t, leaderCtl(st, func(AlertEvent) {}))
	if got := load(t, st, "vol-a"); got.State != storage.StateNeedsManualRecovery || got.Primary != "n1" {
		t.Fatalf("controller modified a diverged volume: %+v", got)
	}
}

func TestNonLeaderIsInert(t *testing.T) {
	st := newStore(t)
	seedMesh(st, "n1", "n2")
	seedVolume(t, st, "vol-a", 2, []string{"n1", "n2"}, "", storage.StateHealthy)
	reconcile(t, New(Options{St: st, IsLeader: func() bool { return false }}))
	if got := load(t, st, "vol-a").Primary; got != "" {
		t.Fatalf("non-leader elected a primary: %q", got)
	}
}

func TestOpsMovePrimaryAndDeleteByName(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedMesh(st, "n1", "n2", "n3")
	seedVolume(t, st, "vol-op", 3, []string{"n1", "n2", "n3"}, "n1", storage.StateHealthy)
	c := leaderCtl(st, func(AlertEvent) {})
	put := func(kind, body string) {
		t.Helper()
		if _, err := st.Put(ctx, store.Key("/volumes/_ops/"+kind+"/vol-op"), []byte(body)); err != nil {
			t.Fatal(err)
		}
	}

	put("move-primary", `{"target":"vol-op","to":"n2"}`)
	reconcile(t, c)
	if got := load(t, st, "vol-op").Primary; got != "n2" {
		t.Fatalf("primary = %s, want n2", got)
	}
	if _, err := st.Get(ctx, "/volumes/_ops/move-primary/vol-op"); err == nil {
		t.Fatal("move-primary op not consumed")
	}

	put("move-primary", `{"target":"vol-op","to":"n9"}`) // n9 holds no replica
	reconcile(t, c)
	if got := load(t, st, "vol-op").Primary; got != "n2" {
		t.Fatalf("primary moved to a node without a replica: %s", got)
	}

	put("delete", `{"target":"vol-op"}`)
	reconcile(t, c)
	if got := load(t, st, "vol-op").State; got != storage.StateDeleting {
		t.Fatalf("state = %s, want Deleting", got)
	}
}

func TestCreatingPrimaryIsNotStolenByAReplicaThatReportedFirst(t *testing.T) {
	st := newStore(t)
	seedMesh(st, "n1", "n2", "n3")
	seedVolume(t, st, "vol-a", 3, []string{"n1", "n2", "n3"}, "n1", storage.StateCreating)
	edit(t, st, "vol-a", func(s *storage.Status) {
		s.Placement[0].Role, s.Placement[0].Healthy = "", false // n1 has not reported yet
	})
	reconcile(t, leaderCtl(st, func(AlertEvent) {}))
	if got := load(t, st, "vol-a").Primary; got != "n1" {
		t.Fatalf("primary = %q: a replica that was still being created lost it", got)
	}
}

func TestStatesTheControllerDoesNotDeriveAreLeftAlone(t *testing.T) {
	st := newStore(t)
	seedMesh(st, "n1", "n2", "n3")
	seedVolume(t, st, "vol-a", 3, []string{"n1", "n2", "n3"}, "n1", storage.StateFailed)
	reconcile(t, leaderCtl(st, func(AlertEvent) {}))
	if got := load(t, st, "vol-a").State; got != storage.StateFailed {
		t.Fatalf("state = %s, want Failed untouched", got)
	}
}
