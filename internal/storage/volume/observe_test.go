package volume

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/storage/drbd"
)

var trio = []drbd.Member{{Host: "n1", NodeID: 0}, {Host: "n2", NodeID: 1}, {Host: "n3", NodeID: 2}}

func observeFixture(t *testing.T, name, self string) Observed {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "test", "fixtures", "drbd", name))
	if err != nil {
		t.Fatal(err)
	}
	st, err := drbd.ParseStatus("r0", b)
	if err != nil {
		t.Fatal(err)
	}
	return Observe(st, self, trio)
}

func replica(t *testing.T, o Observed, host string) Replica {
	t.Helper()
	for _, r := range o.Replicas {
		if r.NodeID == host {
			return r
		}
	}
	t.Fatalf("no replica %q in %+v", host, o.Replicas)
	return Replica{}
}

type want struct {
	role    storage.Role
	healthy bool
}

func check(t *testing.T, o Observed, exp map[string]want) {
	t.Helper()
	if len(o.Replicas) != len(exp) {
		t.Errorf("got %d replicas, want %d: %+v", len(o.Replicas), len(exp), o.Replicas)
	}
	for host, w := range exp {
		if r := replica(t, o, host); r.Role != w.role || r.Healthy != w.healthy {
			t.Errorf("%s = {%q healthy=%v}, want {%q healthy=%v}", host, r.Role, r.Healthy, w.role, w.healthy)
		}
	}
}

func TestObserveHealthyPrimaryShowsEveryReplica(t *testing.T) {
	o := observeFixture(t, "healthy-primary.n1.json", "n1")
	check(t, o, map[string]want{
		"n1": {storage.RolePrimary, true},
		"n2": {storage.RoleSecondary, true},
		"n3": {storage.RoleSecondary, true},
	})
	if !o.Quorum {
		t.Error("healthy volume reports no quorum")
	}
}

func TestObserveFromASecondaryNamesThePrimary(t *testing.T) {
	o := observeFixture(t, "healthy-secondary.n2.json", "n2")
	if r := replica(t, o, "n2"); r.Role != storage.RoleSecondary || !r.Healthy {
		t.Errorf("self = %+v", r)
	}
	primaries := 0
	for _, r := range o.Replicas {
		if r.Role == storage.RolePrimary {
			primaries++
		}
	}
	if primaries != 1 {
		t.Errorf("%d primaries in %+v", primaries, o.Replicas)
	}
}

func TestObserveAnUnreachablePeerHasNoKnownRoleAndIsNotHealthy(t *testing.T) {
	o := observeFixture(t, "degraded.n1.json", "n1")
	check(t, o, map[string]want{
		"n1": {storage.RolePrimary, true},
		"n2": {storage.RoleSecondary, true},
		"n3": {"", false},
	})
}

func TestObserveIgnoresTheStaleDiskStateOfADisconnectedPeer(t *testing.T) {
	o := observeFixture(t, "quorum-lost.n1.json", "n1")
	for _, h := range []string{"n2", "n3"} {
		if r := replica(t, o, h); r.Role != "" || r.Healthy || r.SyncPercent != 0 {
			t.Errorf("%s = %+v, want unknown, unhealthy, no progress", h, r)
		}
	}
}

func TestObserveAPrimaryWithoutQuorumIsNotHealthy(t *testing.T) {
	o := observeFixture(t, "quorum-lost.n1.json", "n1")
	if o.Quorum {
		t.Error("quorum-lost fixture reports quorum")
	}
	if r := replica(t, o, "n1"); r.Role != storage.RolePrimary || r.Healthy {
		t.Errorf("self = %+v, want Primary but unhealthy", r)
	}
}

func TestObserveSyncTargetSeenFromItsSource(t *testing.T) {
	o := observeFixture(t, "syncing-source.n1.json", "n1")
	r := replica(t, o, "n3")
	if r.Role != storage.RoleResyncing || r.Healthy || r.SyncPercent != 75.97 || r.OutOfSyncKiB != 62976 {
		t.Errorf("n3 = %+v", r)
	}
	if n2 := replica(t, o, "n2"); n2.Role != storage.RoleSecondary || !n2.Healthy || n2.SyncPercent != 0 {
		t.Errorf("n2 = %+v, want an idle healthy Secondary", n2)
	}
}

func TestObserveSyncTargetSeesItselfResyncingAndItsSourceHealthy(t *testing.T) {
	o := observeFixture(t, "syncing-target.n3.json", "n3")
	if r := replica(t, o, "n3"); r.Role != storage.RoleResyncing || r.Healthy || r.SyncPercent != 80.17 {
		t.Errorf("self = %+v", r)
	}
	if r := replica(t, o, "n1"); r.Role != storage.RolePrimary || !r.Healthy {
		t.Errorf("source = %+v", r)
	}
	if r := replica(t, o, "n2"); r.Role != "" || r.Healthy {
		t.Errorf("unreachable n2 = %+v", r)
	}
}

func TestObserveDisconnectedFromEveryoneIsStandAlone(t *testing.T) {
	st := &drbd.Status{Role: drbd.RoleSecondary, Volumes: []drbd.Volume{{DiskState: drbd.DiskUpToDate, Quorum: true}}}
	o := Observe(st, "n1", trio)
	check(t, o, map[string]want{
		"n1": {storage.RoleSecondary, true},
		"n2": {"", false},
		"n3": {"", false},
	})
}

func TestObserveNonCurrentDisksAreStale(t *testing.T) {
	for _, disk := range []drbd.DiskState{drbd.DiskInconsistent, "Outdated", "Diskless", "Failed", drbd.DiskUnknown} {
		st := &drbd.Status{Role: drbd.RoleSecondary, Volumes: []drbd.Volume{{DiskState: disk, Quorum: true}}}
		if r := replica(t, Observe(st, "n1", trio), "n1"); r.Role != storage.RoleStale || r.Healthy {
			t.Errorf("disk %s -> %+v, want Stale and unhealthy", disk, r)
		}
	}
}

func TestObserveAStatusWithNoVolumesIsNeverHealthy(t *testing.T) {
	o := Observe(&drbd.Status{Role: drbd.RoleSecondary}, "n1", trio)
	if r := replica(t, o, "n1"); r.Healthy {
		t.Errorf("volume-less self reported healthy: %+v", r)
	}
	if o.Quorum {
		t.Error("volume-less status reported quorum")
	}
}

func TestObserveSkipsPeersThatAreNotMembers(t *testing.T) {
	st := &drbd.Status{
		Role:    drbd.RoleSecondary,
		Volumes: []drbd.Volume{{DiskState: drbd.DiskUpToDate, Quorum: true}},
		Peers:   []drbd.Peer{{NodeID: 9, Name: "gone", Connection: drbd.ConnConnected, Role: drbd.RoleSecondary}},
	}
	o := Observe(st, "n1", trio[:1])
	if len(o.Replicas) != 1 {
		t.Errorf("replicas = %+v, want only the member", o.Replicas)
	}
}

func TestObserveWorstVolumeDecidesDiskState(t *testing.T) {
	st := &drbd.Status{Role: drbd.RoleSecondary, Volumes: []drbd.Volume{
		{DiskState: drbd.DiskUpToDate, Quorum: true}, {DiskState: drbd.DiskInconsistent, Quorum: true},
	}}
	if r := replica(t, Observe(st, "n1", trio[:1]), "n1"); r.Healthy {
		t.Errorf("one bad volume left the replica healthy: %+v", r)
	}
}

func TestObserveOrdersReplicasByNodeIDWhateverOrderMembersArriveIn(t *testing.T) {
	st := &drbd.Status{Role: drbd.RoleSecondary, Volumes: []drbd.Volume{{DiskState: drbd.DiskUpToDate, Quorum: true}}}
	o := Observe(st, "n1", []drbd.Member{trio[2], trio[0], trio[1]})
	for i, want := range []string{"n1", "n2", "n3"} {
		if o.Replicas[i].NodeID != want {
			t.Fatalf("order = %+v", o.Replicas)
		}
	}
}

func TestObserveReportsTheSlowestOfSeveralSyncs(t *testing.T) {
	sync := func(pct float64, oos uint64) drbd.PeerVolume {
		return drbd.PeerVolume{Replication: drbd.ReplSyncTarget, DiskState: drbd.DiskUpToDate, PercentInSync: pct, OutOfSyncKiB: oos}
	}
	st := &drbd.Status{
		Role:    drbd.RoleSecondary,
		Volumes: []drbd.Volume{{DiskState: drbd.DiskInconsistent, Quorum: true}},
		Peers: []drbd.Peer{
			{NodeID: 1, Connection: drbd.ConnConnected, Role: drbd.RolePrimary, Volumes: []drbd.PeerVolume{sync(60, 400), sync(20, 800)}},
			{NodeID: 2, Connection: drbd.ConnConnected, Role: drbd.RoleSecondary, Volumes: []drbd.PeerVolume{sync(90, 100)}},
		},
	}
	if r := replica(t, Observe(st, "n1", trio), "n1"); r.SyncPercent != 20 || r.OutOfSyncKiB != 800 {
		t.Errorf("progress = %v%% / %d KiB, want 20%% / 800 KiB", r.SyncPercent, r.OutOfSyncKiB)
	}
}
