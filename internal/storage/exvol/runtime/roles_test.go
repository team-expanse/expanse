package runtime

import (
	"testing"

	"github.com/expanse/expanse/internal/storage"
)

func placement() []storage.Replica {
	return []storage.Replica{
		{NodeID: "n1", Role: storage.RolePrimary, Healthy: true},
		{NodeID: "n2", Role: storage.RoleStale},
		{NodeID: "n3", Role: storage.RoleSecondary, Healthy: true},
	}
}

func set(ids ...string) map[string]bool {
	m := map[string]bool{}
	for _, id := range ids {
		m[id] = true
	}
	return m
}

// A resync whose outcome was never recorded leaves a healthy replica published Stale
// for good; the coordinator's own view must win.
func TestPublishedStaleReplicaTheCoordinatorHoldsLiveIsRestored(t *testing.T) {
	pl := placement()
	if !reconcileRoles(pl, "n1", set(), set("n2", "n3"), func(string) bool { return false }) {
		t.Fatal("expected a change")
	}
	if pl[1].Role != storage.RoleSecondary || !pl[1].Healthy {
		t.Fatalf("n2 = %+v, want a healthy Secondary", pl[1])
	}
	if pl[2].Role != storage.RoleSecondary || pl[0].Role != storage.RolePrimary {
		t.Fatalf("others must be untouched: %+v", pl)
	}
}

func TestReplicaTheCoordinatorMarkedStaleIsPublishedStale(t *testing.T) {
	pl := placement()
	if !reconcileRoles(pl, "n1", set("n3"), set("n2"), func(string) bool { return false }) {
		t.Fatal("expected a change")
	}
	if pl[2].Role != storage.RoleStale || pl[2].Healthy {
		t.Fatalf("n3 = %+v, want Stale", pl[2])
	}
}

func TestRolesAreLeftAloneWhileAResyncIsRunningOrTheReplicaIsUnknown(t *testing.T) {
	pl := placement()
	busy := func(id string) bool { return id == "n2" }
	if reconcileRoles(pl, "n1", set(), set("n2", "n3"), busy) {
		t.Fatalf("n2 is mid-resync; its role is not ours to flip: %+v", pl)
	}
	// n2 is in neither set: not in the fan-out at all, so adoption (not us) handles it.
	if reconcileRoles(placement(), "n1", set(), set("n3"), func(string) bool { return false }) {
		t.Fatal("a replica the coordinator does not know must be left to the adoption path")
	}
}
