package volume

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/storage/drbd"
)

func tiebreakerAllocation() drbd.Allocation {
	al := allocation()
	al.Diskless = map[string]int{"n3": 4}
	return al
}

func tiebreakerAddrs() map[string]netip.Addr {
	a := addrs()
	a["n3"] = netip.MustParseAddr("192.168.1.3")
	return a
}

func TestFromAllocationAddsTheTiebreakerAsADisklessMember(t *testing.T) {
	d, err := FromAllocation(tiebreakerAllocation(), "n1", 1, true, tiebreakerAddrs())
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(d.Members, func(m drbd.Member) bool { return m.Host == "n3" })
	if i < 0 || !d.Members[i].Diskless || d.Members[i].NodeID != 4 || len(d.Members) != 4 || d.Tiebreaker() {
		t.Errorf("got %+v", d)
	}
}

// A tiebreaker has no bitmap slots to forget and cannot ack, so it is given no retired ids.
func TestFromAllocationForATiebreakerSelf(t *testing.T) {
	d, err := FromAllocation(tiebreakerAllocation(), "n3", 1, true, tiebreakerAddrs())
	if err != nil {
		t.Fatal(err)
	}
	if !d.Tiebreaker() || len(d.Retired) != 0 {
		t.Errorf("got %+v", d)
	}
}

func tiebreakerDesired() Desired {
	d := desired()
	d.Self = "n3"
	d.Members[2].Diskless = true
	return d
}

func TestReconcileATiebreakerOnlyWritesConfigAndComesUp(t *testing.T) {
	r := newRig(t)
	d := tiebreakerDesired()
	d.Retired = []int{5}
	res := reconcile(t, r, d)
	if want := []Action{WriteConfig, Up}; !reflect.DeepEqual(res.Actions, want) {
		t.Errorf("actions %v, want %v", res.Actions, want)
	}
	if got := r.j.mutating(); !reflect.DeepEqual(got, []string{"drbd.up"}) {
		t.Errorf("calls %v, want only up", got)
	}
	body, _ := os.ReadFile(filepath.Join(r.dir, "vol-a1.res"))
	if !strings.Contains(string(body), "on n3 { node-id 2; address 192.168.1.3:7793; disk none; }") {
		t.Errorf("config:\n%s", body)
	}
	r.j.calls = nil
	if again := reconcile(t, r, d); again.Changed() || len(r.j.mutating()) != 0 {
		t.Errorf("second pass acted: %v %v", again.Actions, r.j.calls)
	}
}

func TestPresentSeesATiebreakersConfig(t *testing.T) {
	r := newRig(t)
	reconcile(t, r, tiebreakerDesired())
	if ok, err := r.rt.Present(context.Background(), "vol-a1"); err != nil || !ok {
		t.Fatalf("Present = %v, %v", ok, err)
	}
}

func TestRemoveATiebreakerTakesItDownAndDropsItsConfig(t *testing.T) {
	r := newRig(t)
	reconcile(t, r, tiebreakerDesired())
	r.j.calls = nil
	if err := remove(t, r); err != nil {
		t.Fatal(err)
	}
	if got := r.j.mutating(); !reflect.DeepEqual(got, []string{"drbd.down"}) || configExists(r) {
		t.Errorf("calls %v, config left: %v", got, configExists(r))
	}
	if ok, _ := r.rt.Present(context.Background(), "vol-a1"); ok {
		t.Error("still present after removal")
	}
}

func disklessPeer(id int) drbd.Peer {
	return peerOf(id, drbd.ConnConnected, drbd.ReplEstablished, drbd.DiskDiskless)
}

func TestDecideForcesAFreshResourceWithATiebreaker(t *testing.T) {
	inc := drbd.DiskInconsistent
	st := status(drbd.RoleSecondary, true, inc, peer(drbd.ConnConnected, inc), disklessPeer(2))
	if got := decide(&st, true, true); got != forcePromote {
		t.Errorf("got %v; a tiebreaker holds no data, so it cannot block the first promotion", got)
	}
	st = status(drbd.RoleSecondary, true, inc, peer(drbd.ConnConnecting, inc), disklessPeer(2))
	if got := decide(&st, true, true); got != promote {
		t.Errorf("got %v; an unreachable replica may hold data", got)
	}
}

func TestVerifyIgnoresTheTiebreaker(t *testing.T) {
	r := checked(t, drbd.RolePrimary, drbd.DiskUpToDate, inSync(1), disklessPeer(2))
	if err := r.rt.Verify(context.Background(), "vol-a1"); err != nil {
		t.Fatal(err)
	}
	r = checked(t, drbd.RolePrimary, drbd.DiskUpToDate, disklessPeer(2))
	refused(t, r.rt.Verify(context.Background(), "vol-a1"))
}

func TestResyncNeverCopiesFromTheTiebreaker(t *testing.T) {
	r := checked(t, drbd.RoleSecondary, drbd.DiskUpToDate, disklessPeer(2))
	refused(t, r.rt.Resync(context.Background(), "vol-a1"))
}

// tiebreak makes n1 the volume's tiebreaker while n2 and n3 hold its data.
func (r *nodeRig) tiebreak(t *testing.T) {
	t.Helper()
	r.place(t, "n2", "n2", "n3")
	if _, err := r.alloc.AssignDiskless(context.Background(), vol, "n1"); err != nil {
		t.Fatal(err)
	}
}

func TestSyncConvergesATiebreakerWithoutLeadingOrPublishing(t *testing.T) {
	r := newNodeRig(t)
	r.tiebreak(t)
	r.setPrimary(t, "n1") // even a confused controller cannot make a tiebreaker lead
	behind := peerOf(0, drbd.ConnConnected, drbd.ReplEstablished, drbd.DiskUpToDate)
	behind.Role, behind.Volumes[0].OutOfSyncKiB = drbd.RoleSecondary, 8
	r.drbd.st[vol] = &drbd.Status{Name: vol, Role: drbd.RoleSecondary, Peers: []drbd.Peer{behind},
		Volumes: []drbd.Volume{{DiskState: drbd.DiskDiskless, Quorum: true}}}
	before := r.status(t)
	r.mustSync(t)
	if len(r.conv.desired) != 1 || !r.conv.desired[0].Tiebreaker() || len(r.conv.desired[0].Members) != 3 {
		t.Fatalf("desired = %+v", r.conv.desired)
	}
	if r.lead.isRunning(vol) || len(r.lead.started) != 0 {
		t.Error("a tiebreaker started leading")
	}
	if after := r.status(t); !reflect.DeepEqual(after.Placement, before.Placement) {
		t.Errorf("placement changed: %+v -> %+v", before.Placement, after.Placement)
	}
}

func TestSyncRemovesADroppedTiebreaker(t *testing.T) {
	r := newNodeRig(t)
	r.tiebreak(t)
	r.mustSync(t)
	if _, err := r.alloc.RetireNode(context.Background(), vol, "n1"); err != nil {
		t.Fatal(err)
	}
	r.mustSync(t)
	if !slices.Equal(r.conv.removed, []string{vol}) {
		t.Errorf("removed %v", r.conv.removed)
	}
}

func TestADeletedVolumeTakesItsTiebreakerDown(t *testing.T) {
	r := newNodeRig(t)
	r.tiebreak(t)
	r.mustSync(t)
	r.setState(t, storage.StateDeleting)
	r.mustSync(t)
	if !slices.Equal(r.conv.removed, []string{vol}) {
		t.Errorf("removed %v", r.conv.removed)
	}
}
