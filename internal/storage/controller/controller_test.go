package controller

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/storage/drbd"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/boltstore"
	pb "github.com/expanse/expanse/proto"
	pbproto "google.golang.org/protobuf/proto"
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

// seedSingletonBlock writes a SINGLETON block with one storage entry bound
// by storageName, live/placed on nodes (PHASE-03-TASKS.md D2).
func seedSingletonBlock(t *testing.T, st *boltstore.Store, ns, name, storageName string, nodes ...string) {
	t.Helper()
	blk := &pb.Block{
		Spec: &pb.BlockSpec{
			Strategy: &pb.Strategy{Kind: pb.StrategyKind_SINGLETON},
			Storage:  []*pb.Storage{{Name: storageName}},
		},
	}
	raw, err := pbproto.Marshal(blk)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(context.Background(), store.Key("/blocks/"+ns+"/"+name), raw); err != nil {
		t.Fatal(err)
	}
	status := &pb.BlockStatus{}
	for i, n := range nodes {
		status.Placements = append(status.Placements,
			&pb.PlacementStatus{ReplicaIndex: int32(i), NodeId: n, Phase: pb.Phase_RUNNING})
	}
	sraw, err := pbproto.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(context.Background(), store.Key("/blocks/"+ns+"/"+name+"/status"), sraw); err != nil {
		t.Fatal(err)
	}
}

// seedActiveActiveBlock writes an active-active block (no strategy.kind,
// PHASE-05-TASKS.md D3's plain N-replica shape) with one storage entry,
// one placement per node in nodes (replica index = position in nodes).
func seedActiveActiveBlock(t *testing.T, st *boltstore.Store, ns, name, storageName string, replicas int32, nodes ...string) {
	t.Helper()
	blk := &pb.Block{
		Spec: &pb.BlockSpec{
			Replicas: &replicas,
			Storage:  []*pb.Storage{{Name: storageName, Size: "64Mi"}},
		},
	}
	raw, err := pbproto.Marshal(blk)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(context.Background(), store.Key("/blocks/"+ns+"/"+name), raw); err != nil {
		t.Fatal(err)
	}
	status := &pb.BlockStatus{}
	for i, n := range nodes {
		status.Placements = append(status.Placements,
			&pb.PlacementStatus{ReplicaIndex: int32(i), NodeId: n, Phase: pb.Phase_RUNNING})
	}
	sraw, err := pbproto.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(context.Background(), store.Key("/blocks/"+ns+"/"+name+"/status"), sraw); err != nil {
		t.Fatal(err)
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

func TestDeleteDropsTheVolumesSnapshotRecords(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	seedMesh(st, "n1")
	seedVolume(t, st, "vol-a", 1, nil, "", storage.StateDeleting)
	for _, id := range []string{"vol-a", "vol-ab"} {
		if err := storage.PutSnapshot(ctx, st, id, storage.SnapshotRecord{Name: "s", Node: "n1"}); err != nil {
			t.Fatal(err)
		}
	}
	reconcile(t, leaderCtl(st, func(AlertEvent) {}))
	if recs, _ := storage.ListSnapshotRecords(ctx, st, "vol-a"); len(recs) != 0 {
		t.Errorf("a deleted volume keeps its snapshot records: %+v", recs)
	}
	if recs, _ := storage.ListSnapshotRecords(ctx, st, "vol-ab"); len(recs) != 1 {
		t.Errorf("another volume's records were touched: %+v", recs)
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

func markNode(t *testing.T, st *boltstore.Store, id, record string) {
	t.Helper()
	if _, err := st.Put(context.Background(), store.Key("/nodes/"+id), []byte(record)); err != nil {
		t.Fatal(err)
	}
}

// A cleanly stopped agent releases its liveness lease, so the lease alone cannot say
// the node is gone; the failure monitor's record state must.
func TestPrimaryOnAnUnreachableNodeIsReplaced(t *testing.T) {
	for _, state := range []string{"unreachable", "failed"} {
		st := newStore(t)
		seedMesh(st, "n1", "n2", "n3")
		markNode(t, st, "n1", `{"id":"n1","state":"`+state+`"}`)
		seedVolume(t, st, "vol-a", 3, []string{"n1", "n2", "n3"}, "n1", storage.StateHealthy)
		reconcile(t, leaderCtl(st, func(AlertEvent) {}))
		if got := load(t, st, "vol-a").Primary; got != "n2" {
			t.Errorf("state %q: primary = %q, want n2", state, got)
		}
	}
}

func TestPlacementSkipsUnreachableAndCordonedNodes(t *testing.T) {
	c, st := newPlacer(t, "n1", "n2", "n3", "n4")
	markNode(t, st, "n3", `{"id":"n3","state":"unreachable"}`)
	markNode(t, st, "n4", `{"id":"n4","cordoned":true}`)
	request(t, st, "data", 3)
	c.processPending(context.Background(), meshed(t, c))
	if len(placed(t, st)) != 0 {
		t.Error("placed on an unreachable or cordoned node")
	}
}

// TestBlockBoundVolumePrimaryMovesToTheBlocksHost is the regression test for
// PHASE-03-TASKS.md D2: a SINGLETON block's bound volume gets its primary
// moved onto whichever node the block is actually placed on, not left
// wherever it happened to be — this was movePrimaryForBlock's whole
// documented purpose, but it only ever logged the decision without writing
// it until this fix.
func TestBlockBoundVolumePrimaryMovesToTheBlocksHost(t *testing.T) {
	st := newStore(t)
	seedMesh(st, "n1", "n2", "n3")
	vname := storage.BlockVolumeName("default", "share", "share-data")
	seedVolume(t, st, vname, 3, []string{"n1", "n2", "n3"}, "n1", storage.StateHealthy)
	seedSingletonBlock(t, st, "default", "share", "share-data", "n2")

	reconcile(t, leaderCtl(st, func(AlertEvent) {}))

	if got := load(t, st, vname).Primary; got != "n2" {
		t.Fatalf("primary = %q, want n2 (the block's host)", got)
	}
}

// The primary is left alone, and no store write happens, once it is
// already co-located with the block.
func TestBlockBoundVolumePrimaryAlreadyCoLocatedIsLeftAlone(t *testing.T) {
	st := newStore(t)
	seedMesh(st, "n1", "n2", "n3")
	vname := storage.BlockVolumeName("default", "share", "share-data")
	seedVolume(t, st, vname, 3, []string{"n1", "n2", "n3"}, "n1", storage.StateHealthy)
	seedSingletonBlock(t, st, "default", "share", "share-data", "n1")
	_, before, _ := storage.LoadStatus(context.Background(), st, vname)

	reconcile(t, leaderCtl(st, func(AlertEvent) {}))

	if got := load(t, st, vname).Primary; got != "n1" {
		t.Fatalf("primary = %q, want n1 unchanged", got)
	}
	if _, after, _ := storage.LoadStatus(context.Background(), st, vname); after != before {
		t.Fatalf("revision %d -> %d: rewrote a status already co-located", before, after)
	}
}

// A node hosting the block but whose local replica is unhealthy is not a
// candidate — matching electPrimary's own bar (election.go's firstCandidate)
// so a bad choice never costs correctness, only time.
func TestBlockBoundVolumeNeverMovesPrimaryToAnUnhealthyReplica(t *testing.T) {
	st := newStore(t)
	seedMesh(st, "n1", "n2", "n3")
	vname := storage.BlockVolumeName("default", "share", "share-data")
	seedVolume(t, st, vname, 3, []string{"n1", "n2", "n3"}, "n1", storage.StateHealthy)
	edit(t, st, vname, func(s *storage.Status) {
		for i := range s.Placement {
			if s.Placement[i].NodeID == "n2" {
				s.Placement[i].Healthy = false
			}
		}
	})
	seedSingletonBlock(t, st, "default", "share", "share-data", "n2")

	reconcile(t, leaderCtl(st, func(AlertEvent) {}))

	if got := load(t, st, vname).Primary; got != "n1" {
		t.Fatalf("primary = %q, want n1 unchanged (n2's replica is unhealthy)", got)
	}
}

// PHASE-05-TASKS.md D3: an active-active block's storage entry must not
// collapse to a single shared volume the way SINGLETON/DAEMONSET's does —
// each of its N replicas gets its own independently-named, independently
// requested volume.
func TestActiveActiveBlockRequestsOneVolumePerReplica(t *testing.T) {
	st := newStore(t)
	seedMesh(st, "n1", "n2", "n3")
	seedActiveActiveBlock(t, st, "default", "pg", "pgdata", 3, "n1", "n2", "n3")

	reconcile(t, leaderCtl(st, func(AlertEvent) {}))

	for i := 0; i < 3; i++ {
		vname := storage.BlockReplicaVolumeName("default", "pg", "pgdata", i)
		if _, err := st.Get(context.Background(), storage.PendingCreateKey(vname)); err != nil {
			t.Fatalf("replica %d: no pending create for %q: %v", i, vname, err)
		}
	}
	// The old shared-volume name (as a SINGLETON/DAEMONSET block of the
	// same name would use) must never itself be requested.
	shared := storage.BlockVolumeName("default", "pg", "pgdata")
	if _, err := st.Get(context.Background(), storage.PendingCreateKey(shared)); err == nil {
		t.Fatalf("a shared composite volume %q was requested; active-active must not collapse replicas onto one volume", shared)
	}
}

// Each replica's own volume must co-locate with THAT replica's node only —
// never with a sibling replica's node, which would defeat D3's whole point
// (independent, non-contending per-replica storage).
func TestActiveActiveReplicaVolumePrimaryCoLocatesOnlyWithItsOwnReplica(t *testing.T) {
	st := newStore(t)
	seedMesh(st, "n1", "n2", "n3")
	v0 := storage.BlockReplicaVolumeName("default", "pg", "pgdata", 0)
	v1 := storage.BlockReplicaVolumeName("default", "pg", "pgdata", 1)
	// Both volumes seeded with n3 as primary — reconcile must move v0's
	// primary to replica 0's node (n1) and v1's to replica 1's node (n2),
	// never to each other's or a third node.
	seedVolume(t, st, v0, 3, []string{"n1", "n2", "n3"}, "n3", storage.StateHealthy)
	seedVolume(t, st, v1, 3, []string{"n1", "n2", "n3"}, "n3", storage.StateHealthy)
	seedActiveActiveBlock(t, st, "default", "pg", "pgdata", 2, "n1", "n2")

	reconcile(t, leaderCtl(st, func(AlertEvent) {}))

	if got := load(t, st, v0).Primary; got != "n1" {
		t.Fatalf("replica 0's volume primary = %q, want n1 (its own replica's node)", got)
	}
	if got := load(t, st, v1).Primary; got != "n2" {
		t.Fatalf("replica 1's volume primary = %q, want n2 (its own replica's node)", got)
	}
}
