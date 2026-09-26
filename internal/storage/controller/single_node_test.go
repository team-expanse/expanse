package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	pbproto "google.golang.org/protobuf/proto"

	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/boltstore"
	pb "github.com/expanse/expanse/proto"
)

// placedOne places the only pending request and returns its spec and status.
func placedOne(t *testing.T, c *Controller, st store.Store) (storage.Spec, storage.Status) {
	t.Helper()
	ctx := context.Background()
	c.processPending(ctx, meshed(t, c))
	ids := placed(t, st)
	if len(ids) != 1 {
		t.Fatalf("placed %v, want one volume", ids)
	}
	spec, err := storage.LoadSpec(ctx, st, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	status, _, err := storage.LoadStatus(ctx, st, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	return spec, status
}

func TestDefaultRequestOnOneNodePlacesOneReplicaAndKeepsTheTarget(t *testing.T) {
	c, st := newPlacer(t, "n1")
	request(t, st, "data", 0)
	spec, status := placedOne(t, c, st)
	if spec.Replication != 3 || len(status.Placement) != 1 || status.Primary != "n1" {
		t.Fatalf("spec = %+v, status = %+v; want target 3 on n1 alone", spec, status)
	}
}

func TestDefaultRequestClampsToTheNodesThereAre(t *testing.T) {
	c, st := newPlacer(t, "n1", "n2")
	request(t, st, "data", 0)
	spec, status := placedOne(t, c, st)
	if spec.Replication != 3 || len(status.Placement) != 2 {
		t.Fatalf("spec = %+v, status = %+v; want target 3 on two nodes", spec, status)
	}
}

func TestUnplaceableExplicitRequestRecordsWhyUntilItPlaces(t *testing.T) {
	ctx := context.Background()
	c, st := newPlacer(t, "n1")
	request(t, st, "data", 3)
	c.processPending(ctx, meshed(t, c))
	e, err := st.Get(ctx, storage.PlacementReasonKey("data"))
	if err != nil || !strings.Contains(string(e.Value), "needs 3") || !strings.Contains(string(e.Value), "1 eligible") {
		t.Fatalf("reason = %q, %v; want it to name the 3 needed and 1 eligible", e.Value, err)
	}
	seedMesh(st, "n2", "n3")
	c.processPending(ctx, meshed(t, c))
	if len(placed(t, st)) != 1 {
		t.Fatal("request not placed once enough nodes joined")
	}
	if _, err := st.Get(ctx, storage.PlacementReasonKey("data")); err == nil {
		t.Error("reason left behind after the volume was placed")
	}
}

func TestNamedClassIsHonored(t *testing.T) {
	c, st := newPlacer(t, "n1", "n2", "n3")
	seedClasses(t, st, "storageClasses:\n- {name: scratch, driver: drbd, replication: 1}\n")
	requestClass(t, st, "data", "scratch")
	spec, status := placedOne(t, c, st)
	if spec.Replication != 1 || len(status.Placement) != 1 {
		t.Fatalf("spec = %+v, status = %+v; want the class's replication 1", spec, status)
	}
}

func TestConfiguredDefaultClassReplacesTheBuiltIn(t *testing.T) {
	c, st := newPlacer(t, "n1", "n2", "n3")
	seedClasses(t, st, "storageClasses:\n- {name: default, driver: drbd, replication: 2}\n")
	request(t, st, "data", 0)
	spec, status := placedOne(t, c, st)
	if spec.Replication != 2 || len(status.Placement) != 2 {
		t.Fatalf("spec = %+v, status = %+v; want the configured default's 2", spec, status)
	}
}

func TestDerivedStateCountsMembersAgainstTheTarget(t *testing.T) {
	for _, tc := range []struct {
		healthy, members, target int
		want                     storage.VolumeState
	}{
		{1, 1, 3, storage.StateUnderReplicated}, // one node by design: writable, no redundancy
		{0, 1, 3, storage.StateReadOnly},
		{2, 2, 3, storage.StateUnderReplicated},
		{1, 2, 3, storage.StateDegraded}, // a member was lost, not merely missing
		{3, 3, 3, storage.StateHealthy},
		{2, 3, 3, storage.StateDegraded},
		{1, 3, 3, storage.StateReadOnly},
	} {
		if got := derivedState(tc.healthy, tc.members, tc.target); got != tc.want {
			t.Errorf("derivedState(%d healthy, %d members, target %d) = %s, want %s",
				tc.healthy, tc.members, tc.target, got, tc.want)
		}
	}
}

func TestLoneReplicaWithNoSpareIsUnderReplicatedWithoutAlerting(t *testing.T) {
	st := newStore(t)
	seedMesh(st, "n1")
	seedVolume(t, st, "vol-a", 3, []string{"n1"}, "n1", storage.StateCreating)
	var alerts []AlertEvent
	reconcile(t, leaderCtl(st, func(ev AlertEvent) { alerts = append(alerts, ev) }))
	if got := load(t, st, "vol-a").State; got != storage.StateUnderReplicated {
		t.Fatalf("state = %s, want UnderReplicated", got)
	}
	if len(alerts) != 0 {
		t.Fatalf("alerts = %+v; nothing can fix this until a node joins", alerts)
	}
}

func TestUnderReplicatedVolumeAlertsOnceASpareExists(t *testing.T) {
	st := newStore(t)
	seedMesh(st, "n1", "n2")
	seedVolume(t, st, "vol-a", 3, []string{"n1"}, "n1", storage.StateUnderReplicated)
	var alerts []AlertEvent
	reconcile(t, leaderCtl(st, func(ev AlertEvent) { alerts = append(alerts, ev) }))
	if len(alerts) == 0 || alerts[0].Have != 1 || alerts[0].Want != 3 {
		t.Fatalf("alerts = %+v, want under-replication 1/3", alerts)
	}
}

func TestLoneReplicaGrowsOneSyncedMemberAtATime(t *testing.T) {
	r := newRig(t)
	seedMesh(r.st, "n1")
	r.volume("vol-a", 3, "n1")
	r.round(0)
	if got := load(t, r.st, "vol-a").State; got != storage.StateUnderReplicated {
		t.Fatalf("state = %s, want UnderReplicated", got)
	}
	seedMesh(r.st, "n2", "n3", "n4")
	for want := 2; want <= 3; want++ {
		r.round(time.Second)
		r.round(time.Second)
		if got := r.hosts("vol-a"); len(got) != want {
			t.Fatalf("hosts = %v, want %d: the newcomer must sync before the next joins", got, want)
		}
		markAllHealthy(t, r.st, "vol-a")
	}
	r.round(time.Second)
	if got := r.hosts("vol-a"); len(got) != 3 {
		t.Fatalf("hosts = %v, grew past the target", got)
	}
	if got := load(t, r.st, "vol-a").State; got != storage.StateHealthy {
		t.Fatalf("state = %s, want Healthy at 3 of 3", got)
	}
}

func markAllHealthy(t *testing.T, st *boltstore.Store, id string) {
	t.Helper()
	edit(t, st, id, func(s *storage.Status) {
		for i := range s.Placement {
			s.Placement[i].Healthy = true
		}
	})
}

func requestClass(t *testing.T, st store.Store, name, class string) {
	t.Helper()
	raw, err := pbproto.Marshal(&pb.VolumeSpec{Name: name, SizeBytes: 64 << 20, Class: class})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(context.Background(), storage.PendingCreateKey(name), raw); err != nil {
		t.Fatal(err)
	}
}

func seedClasses(t *testing.T, st store.Store, yaml string) {
	t.Helper()
	if _, err := st.Put(context.Background(), storage.StorageClassesKey, []byte(yaml)); err != nil {
		t.Fatal(err)
	}
}
