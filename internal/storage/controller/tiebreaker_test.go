package controller

import (
	"context"
	"maps"
	"slices"
	"testing"

	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/store"
)

func (r *rebuildRig) tiebreakers(id string) []string {
	r.t.Helper()
	return slices.Sorted(maps.Keys(r.allocation(id).Diskless))
}

func (r *rebuildRig) assignTiebreaker(id, host string) {
	r.t.Helper()
	if _, err := r.alloc.AssignDiskless(context.Background(), id, host); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rebuildRig) unmesh(node string) {
	r.t.Helper()
	if err := r.st.Delete(context.Background(), store.Key("/nodes/"+node+"/network.wgPublicKey"), 0); err != nil {
		r.t.Fatal(err)
	}
}

// settle acknowledges every retired id, as the replicas' agents would, then runs a round.
func (r *rebuildRig) settle() {
	r.t.Helper()
	for _, nodeID := range r.allocation("vol-a").Retired {
		r.acknowledgeForgotten("vol-a", nodeID)
	}
	r.round(0)
}

func TestTwoReplicaVolumeGetsATiebreakerOnASpareNode(t *testing.T) {
	r := newRig(t)
	seedMesh(r.st, "n1", "n2", "n3")
	r.volume("vol-a", 2, "n1", "n2")
	r.round(0)
	if got := r.tiebreakers("vol-a"); !slices.Equal(got, []string{"n3"}) {
		t.Fatalf("tiebreakers %v, want [n3]", got)
	}
	if got := r.hosts("vol-a"); !slices.Equal(got, []string{"n1", "n2"}) {
		t.Errorf("a tiebreaker holds no replica, so it has no placement row: %v", got)
	}
	r.round(0)
	if got := r.tiebreakers("vol-a"); len(got) != 1 {
		t.Errorf("a second round must not add another: %v", got)
	}
}

func TestNoTiebreakerWithoutASpareNode(t *testing.T) {
	r := newRig(t)
	seedMesh(r.st, "n1", "n2", "n3", "n4")
	markNode(t, r.st, "n3", `{"role":"witness"}`)
	markNode(t, r.st, "n4", `{"cordoned":true}`)
	r.volume("vol-a", 2, "n1", "n2")
	r.round(0)
	if got := r.tiebreakers("vol-a"); len(got) != 0 {
		t.Errorf("witnesses and cordoned nodes take no tiebreaker: %v", got)
	}
}

func TestOnlyTwoReplicaVolumesGetATiebreaker(t *testing.T) {
	r := newRig(t)
	seedMesh(r.st, "n1", "n2", "n3", "n4")
	r.volume("vol-a", 3, "n1", "n2", "n3")
	r.volume("vol-b", 1, "n1")
	r.round(0)
	for _, id := range []string{"vol-a", "vol-b"} {
		if got := r.tiebreakers(id); len(got) != 0 {
			t.Errorf("%s got tiebreakers %v", id, got)
		}
	}
}

// Turning DRBD quorum on while a replica is behind or away could leave the primary without it.
func TestNoTiebreakerWhileAReplicaIsUnhealthyOrAway(t *testing.T) {
	r := newRig(t)
	seedMesh(r.st, "n1", "n3")
	r.volume("vol-a", 2, "n1", "n2")
	r.volume("vol-b", 2, "n1", "n3")
	edit(t, r.st, "vol-b", func(s *storage.Status) { s.Placement[1].Healthy = false })
	r.round(0)
	for _, id := range []string{"vol-a", "vol-b"} {
		if got := r.tiebreakers(id); len(got) != 0 {
			t.Errorf("%s got tiebreakers %v", id, got)
		}
	}
}

func TestLostTiebreakerIsReplaced(t *testing.T) {
	r := newRig(t)
	seedMesh(r.st, "n1", "n2", "n4")
	r.volume("vol-a", 2, "n1", "n2")
	r.assignTiebreaker("vol-a", "n3") // n3 is down
	r.round(0)
	if got := r.tiebreakers("vol-a"); !slices.Equal(got, []string{"n3"}) {
		t.Fatalf("a node that has only just gone is not lost yet: %v", got)
	}
	r.round(lostAfter)
	if al := r.allocation("vol-a"); len(al.Diskless) != 0 || len(al.Retired) != 1 {
		t.Fatalf("want n3 retired: %+v", al)
	}
	r.settle()
	if got := r.tiebreakers("vol-a"); !slices.Equal(got, []string{"n4"}) {
		t.Errorf("tiebreakers %v, want [n4]", got)
	}
}

// A dead tiebreaker still lets the two replicas outvote a partition; dropping it would not.
func TestLostTiebreakerIsKeptWithoutAReplacement(t *testing.T) {
	r := newRig(t)
	seedMesh(r.st, "n1", "n2")
	r.volume("vol-a", 2, "n1", "n2")
	r.assignTiebreaker("vol-a", "n3")
	r.round(0)
	r.round(lostAfter)
	if got := r.tiebreakers("vol-a"); !slices.Equal(got, []string{"n3"}) {
		t.Errorf("tiebreakers %v, want [n3]", got)
	}
}

// On three nodes the tiebreaker's node is the only place a lost replica can go.
func TestTiebreakerMakesWayForAReplacementReplica(t *testing.T) {
	r := newRig(t)
	seedMesh(r.st, "n1", "n3")
	r.volume("vol-a", 2, "n1", "n2")
	r.assignTiebreaker("vol-a", "n3")
	r.round(0)
	r.round(lostAfter) // n2 is retired
	for range 3 {
		r.settle()
	}
	al := r.allocation("vol-a")
	if !slices.Equal(hostsOf(al), []string{"n1", "n3"}) || len(al.Diskless) != 0 {
		t.Errorf("want n3 a replica and no tiebreaker: %+v", al)
	}
}

func TestThirdReplicaReplacesTheTiebreaker(t *testing.T) {
	r := newRig(t)
	seedMesh(r.st, "n1", "n2", "n3")
	r.volume("vol-a", 3, "n1", "n2")
	r.assignTiebreaker("vol-a", "n3")
	for range 3 {
		r.settle()
	}
	al := r.allocation("vol-a")
	if !slices.Equal(hostsOf(al), []string{"n1", "n2", "n3"}) || len(al.Diskless) != 0 {
		t.Errorf("want three replicas and no tiebreaker: %+v", al)
	}
}

// The tiebreaker's node has no placement row to drop, so a deletion cannot wait on it.
func TestDeleteMarksTheTiebreakerGone(t *testing.T) {
	r := newRig(t)
	seedMesh(r.st, "n1", "n2", "n3")
	r.volume("vol-a", 2, "n1", "n2")
	r.round(0)
	if err := r.c.Delete(context.Background(), "vol-a"); err != nil {
		t.Fatal(err)
	}
	edit(t, r.st, "vol-a", func(s *storage.Status) { s.Placement = nil })
	r.round(0)
	if got := r.gone("n3"); !slices.Equal(got, []string{"vol-a"}) {
		t.Errorf("gone marks for n3: %v", got)
	}
}

func TestDerivedStateCountsTheTiebreakersVote(t *testing.T) {
	for _, tc := range []struct {
		healthy int
		tb      vote
		want    storage.VolumeState
	}{
		{2, tiebreakerDown, storage.StateHealthy},
		{1, tiebreakerUp, storage.StateDegraded},
		{1, tiebreakerDown, storage.StateReadOnly}, // one of three votes: DRBD stops writes
		{1, noTiebreaker, storage.StateDegraded},
	} {
		if got := derivedState(tc.healthy, 2, 2, tc.tb); got != tc.want {
			t.Errorf("derivedState(%d healthy, tiebreaker %v) = %s, want %s", tc.healthy, tc.tb, got, tc.want)
		}
	}
}
