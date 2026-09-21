package controller

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/storage/drbd"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/boltstore"
)

const lostAfter = 10 * time.Minute

// rebuildRig is a controller with a clock the test moves.
type rebuildRig struct {
	t     *testing.T
	st    *boltstore.Store
	c     *Controller
	alloc *drbd.Allocator
	now   time.Time
}

func newRig(t *testing.T) *rebuildRig {
	t.Helper()
	st := newStore(t)
	r := &rebuildRig{t: t, st: st, alloc: drbd.NewAllocator(st, drbd.DefaultMinors, drbd.DefaultPorts), now: time.Unix(1_000_000, 0)}
	r.c = New(Options{
		St: st, Alloc: r.alloc, IsLeader: func() bool { return true }, Alert: func(AlertEvent) {},
		LostAfter: lostAfter, Now: func() time.Time { return r.now },
	})
	return r
}

// volume seeds a healthy volume and gives each host a node-id, as placement does.
func (r *rebuildRig) volume(id string, repl int, hosts ...string) {
	r.t.Helper()
	seedVolume(r.t, r.st, id, repl, hosts, hosts[0], storage.StateHealthy)
	if _, err := r.alloc.Allocate(context.Background(), id); err != nil {
		r.t.Fatal(err)
	}
	for _, h := range hosts {
		if _, err := r.alloc.AssignNodeID(context.Background(), id, h); err != nil {
			r.t.Fatal(err)
		}
	}
}

func (r *rebuildRig) round(after time.Duration) {
	r.t.Helper()
	r.now = r.now.Add(after)
	reconcile(r.t, r.c)
}

func (r *rebuildRig) allocation(id string) drbd.Allocation {
	r.t.Helper()
	al, err := r.alloc.Get(context.Background(), id)
	if err != nil {
		r.t.Fatal(err)
	}
	return al
}

// hosts are the nodes of the placement, sorted.
func (r *rebuildRig) hosts(id string) []string {
	var out []string
	for _, p := range load(r.t, r.st, id).Placement {
		out = append(out, p.NodeID)
	}
	slices.Sort(out)
	return out
}

func (r *rebuildRig) acknowledgeForgotten(id string, nodeID int) {
	r.t.Helper()
	for host := range r.allocation(id).NodeIDs {
		if err := r.alloc.AckForgotten(context.Background(), id, nodeID, host); err != nil {
			r.t.Fatal(err)
		}
	}
}

func hostsOf(al drbd.Allocation) []string {
	out := make([]string, 0, len(al.NodeIDs))
	for h := range al.NodeIDs {
		out = append(out, h)
	}
	slices.Sort(out)
	return out
}

// A replica on a node that stays gone is dropped, its node-id retired, once a spare exists.
func TestReplicaOfANodeGoneForLongEnoughIsRetired(t *testing.T) {
	r := newRig(t)
	seedMesh(r.st, "n1", "n3") // n2 is gone; n3 is a spare
	r.volume("vol-a", 2, "n1", "n2")
	r.round(0) // the controller first sees n2 down
	if got := r.hosts("vol-a"); !slices.Equal(got, []string{"n1", "n2"}) {
		t.Fatalf("a node that has only just gone is not lost yet: %v", got)
	}
	r.round(lostAfter)
	al := r.allocation("vol-a")
	if got := r.hosts("vol-a"); !slices.Equal(got, []string{"n1"}) || !slices.Equal(hostsOf(al), []string{"n1"}) || !slices.Equal(al.Retired, []int{1}) {
		t.Fatalf("want n2 dropped and its id 1 retired: placement %v, allocation %+v", got, al)
	}
}

func TestNodeThatReturnsInTimeIsNotRetiredAndItsClockRestarts(t *testing.T) {
	r := newRig(t)
	seedMesh(r.st, "n1", "n3")
	r.volume("vol-a", 2, "n1", "n2")
	r.round(0)
	r.round(lostAfter - time.Second)
	seedMesh(r.st, "n2") // back
	r.round(time.Second)
	r.round(lostAfter)
	if err := r.st.Delete(context.Background(), store.Key("/nodes/n2/network.wgPublicKey"), 0); err != nil {
		t.Fatal(err)
	}
	r.round(0) // down again: a fresh wait begins
	r.round(lostAfter - time.Second)
	if got := r.hosts("vol-a"); !slices.Equal(got, []string{"n1", "n2"}) {
		t.Fatalf("the wait must restart when the node returns: %v", got)
	}
}

// Retiring a replica nothing can replace would only throw away a copy that may return.
func TestNothingIsRetiredWithoutASpareNode(t *testing.T) {
	r := newRig(t)
	seedMesh(r.st, "n1", "n2") // n3 is gone and there is no fourth node
	r.volume("vol-a", 3, "n1", "n2", "n3")
	r.round(0)
	r.round(lostAfter)
	if got := r.hosts("vol-a"); len(got) != 3 || len(r.allocation("vol-a").Retired) != 0 {
		t.Fatalf("nothing may be dropped without a replacement: %v", got)
	}
}

func TestSpareThatCannotHoldAReplicaDoesNotCount(t *testing.T) {
	r := newRig(t)
	seedMesh(r.st, "n1", "n3")
	markNode(t, r.st, "n3", `{"role":"witness"}`)
	r.volume("vol-a", 2, "n1", "n2")
	r.round(0)
	r.round(lostAfter)
	if len(r.allocation("vol-a").Retired) != 0 {
		t.Fatal("a witness has no capacity and cannot be the replacement")
	}
}

// With no current copy left there is nothing to rebuild from.
func TestNothingIsRetiredWithoutAHealthyReplicaToCopyFrom(t *testing.T) {
	r := newRig(t)
	seedMesh(r.st, "n1", "n3")
	r.volume("vol-a", 2, "n1", "n2")
	edit(t, r.st, "vol-a", func(s *storage.Status) { s.Placement[0].Role, s.Placement[0].Healthy = storage.RoleStale, false })
	r.round(0)
	r.round(lostAfter)
	if got := r.hosts("vol-a"); len(got) != 2 || len(r.allocation("vol-a").Retired) != 0 {
		t.Fatalf("no replica is current, so none may be dropped: %v", got)
	}
}

// A dead unforgotten peer still counts toward quorum (B3 spike), so the replacement joins
// only after every survivor has forgotten the retired id.
func TestReplacementWaitsForEverySurvivorToForget(t *testing.T) {
	r := newRig(t)
	seedMesh(r.st, "n1", "n2", "n4") // n3 is gone; n4 is a spare
	r.volume("vol-a", 3, "n1", "n2", "n3")
	r.round(0)
	r.round(lostAfter)
	if r.hosts("vol-a") == nil || len(r.hosts("vol-a")) != 2 {
		t.Fatalf("n3 should be dropped: %v", r.hosts("vol-a"))
	}
	r.round(time.Second)
	if got := r.hosts("vol-a"); len(got) != 2 {
		t.Fatalf("the replacement joined before the retired id was forgotten: %v", got)
	}
	if err := r.alloc.AckForgotten(context.Background(), "vol-a", 2, "n1"); err != nil {
		t.Fatal(err)
	}
	r.round(time.Second)
	if got := r.hosts("vol-a"); len(got) != 2 {
		t.Fatalf("one survivor has not acknowledged yet: %v", got)
	}
	r.acknowledgeForgotten("vol-a", 2)
	r.round(time.Second)
	if got := r.hosts("vol-a"); !slices.Equal(got, []string{"n1", "n2", "n4"}) {
		t.Fatalf("want n4 as the replacement, got %v", got)
	}
}

func TestReplacementTakesAFreshNodeIDNeverTheDeadOne(t *testing.T) {
	r := newRig(t)
	seedMesh(r.st, "n1", "n2", "n4")
	r.volume("vol-a", 3, "n1", "n2", "n3")
	r.round(0)
	r.round(lostAfter)
	r.acknowledgeForgotten("vol-a", 2)
	r.round(time.Second)
	al := r.allocation("vol-a")
	if al.NodeIDs["n4"] != 3 || al.NodeIDs["n1"] != 0 || al.NodeIDs["n2"] != 1 {
		t.Fatalf("live peers keep their ids and the newcomer takes a fresh one: %v", al.NodeIDs)
	}
}

// A volume left short by an earlier retire gets its replacement as soon as a spare exists.
func TestUnderReplicatedVolumeIsFilledWhenASpareAppears(t *testing.T) {
	r := newRig(t)
	seedMesh(r.st, "n1", "n2")
	r.volume("vol-a", 3, "n1", "n2")
	r.round(0)
	if got := r.hosts("vol-a"); len(got) != 2 {
		t.Fatalf("no spare yet: %v", got)
	}
	seedMesh(r.st, "n3")
	r.round(time.Second)
	if got := r.hosts("vol-a"); !slices.Equal(got, []string{"n1", "n2", "n3"}) {
		t.Fatalf("want n3 added, got %v", got)
	}
}

// Both writes are separate; a crash between them must heal on the next round.
func TestPlacementIsBroughtInLineWithTheAllocation(t *testing.T) {
	r := newRig(t)
	seedMesh(r.st, "n1", "n2", "n4")
	r.volume("vol-a", 3, "n1", "n2", "n3")
	ctx := context.Background()
	if _, err := r.alloc.RetireNode(ctx, "vol-a", "n3"); err != nil { // crashed before the status write
		t.Fatal(err)
	}
	r.acknowledgeForgotten("vol-a", 2)
	if _, err := r.alloc.AssignNodeID(ctx, "vol-a", "n4"); err != nil { // and before this one
		t.Fatal(err)
	}
	r.round(0)
	if got := r.hosts("vol-a"); !slices.Equal(got, []string{"n1", "n2", "n4"}) {
		t.Fatalf("placement must follow the allocation: %v", got)
	}
}

func TestOneReplicaIsReplacedAtATime(t *testing.T) {
	r := newRig(t)
	seedMesh(r.st, "n1", "n4", "n5") // n2 and n3 are gone
	r.volume("vol-a", 3, "n1", "n2", "n3")
	r.round(0)
	r.round(lostAfter)
	if got := r.allocation("vol-a").Retired; len(got) != 1 {
		t.Fatalf("retired %v, want one id until it is forgotten", got)
	}
}

func TestDeadPrimaryIsElectedAwayThenRebuilt(t *testing.T) {
	r := newRig(t)
	seedMesh(r.st, "n2", "n3", "n4") // n1, the primary, is gone
	r.volume("vol-a", 3, "n1", "n2", "n3")
	r.round(0)
	if got := load(t, r.st, "vol-a").Primary; got != "n2" {
		t.Fatalf("primary %q, want n2 elected at once", got)
	}
	r.round(lostAfter)
	if got := r.hosts("vol-a"); !slices.Equal(got, []string{"n2", "n3"}) {
		t.Fatalf("want the dead primary's replica dropped: %v", got)
	}
}

func TestVolumesNotInASteadyStateAreNotRebuilt(t *testing.T) {
	for _, state := range []storage.VolumeState{storage.StateCreating, storage.StateDeleting, storage.StateNeedsManualRecovery} {
		r := newRig(t)
		seedMesh(r.st, "n1", "n3")
		r.volume("vol-a", 2, "n1", "n2")
		edit(t, r.st, "vol-a", func(s *storage.Status) { s.State = state })
		r.round(0)
		r.round(lostAfter)
		if len(r.allocation("vol-a").Retired) != 0 {
			t.Errorf("%s: a replica was retired", state)
		}
	}
}

func TestNoAllocatorMeansNoRebuild(t *testing.T) {
	r := newRig(t)
	seedMesh(r.st, "n1", "n3")
	r.volume("vol-a", 2, "n1", "n2")
	r.c.opts.Alloc = nil
	r.round(0)
	r.round(lostAfter)
	if got := r.hosts("vol-a"); len(got) != 2 {
		t.Fatalf("%v", got)
	}
}
