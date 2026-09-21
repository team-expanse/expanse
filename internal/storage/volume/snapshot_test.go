package volume

import (
	"context"
	"errors"
	"reflect"
	"testing"

	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/storage/drbd"
	"github.com/expanse/expanse/internal/storage/lvm"
)

func (f *fakeLVM) Snapshot(_ context.Context, vg, origin, name string) error {
	f.j.add("lvm.snapshot %s %s", origin, name)
	if f.err != nil {
		return f.err
	}
	src := f.lvs[origin]
	f.lvs[name] = lvm.LV{Name: name, VG: vg, Size: src.Size, Type: lvm.Thin, Pool: src.Pool, Origin: origin}
	return nil
}

func (f *fakeLVM) Activate(_ context.Context, vg, name string) error {
	f.j.add("lvm.activate %s", name)
	return nil
}

func (f *fakeLVM) List(context.Context, string) ([]lvm.LV, error) {
	f.j.add("lvm.list")
	var out []lvm.LV
	for _, lv := range f.lvs {
		out = append(out, lv)
	}
	return out, nil
}

type copyCall struct {
	src, dst string
	n        uint64
}

// fakeCopier records a copy and fails it on demand.
type fakeCopier struct {
	calls []copyCall
	err   error
}

func (c *fakeCopier) copy(_ context.Context, src, dst string, n uint64) error {
	c.calls = append(c.calls, copyCall{src, dst, n})
	return c.err
}

func snapRig(t *testing.T) (*rig, *fakeCopier) {
	t.Helper()
	r := newRig(t)
	reconcile(t, r, desired())
	r.drbd.role = drbd.RolePrimary
	c := &fakeCopier{}
	r.rt.Copy = c.copy
	r.j.calls = nil
	return r, c
}

func TestSnapshotTakesAThinSnapshotOfTheBackingLV(t *testing.T) {
	r, _ := snapRig(t)
	if err := r.rt.Snapshot(context.Background(), "vol-a1", "before"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"lvm.snapshot vol-a1 vol-a1-snap-before"}; !reflect.DeepEqual(r.j.mutating(), want) {
		t.Errorf("calls %v, want %v", r.j.mutating(), want)
	}
}

func TestSnapshotOfTheSameNameTwiceIsANoOp(t *testing.T) {
	r, _ := snapRig(t)
	for range 2 {
		if err := r.rt.Snapshot(context.Background(), "vol-a1", "before"); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(r.j.mutating()); n != 1 {
		t.Errorf("%d mutations, want the one snapshot: %v", n, r.j.mutating())
	}
}

func TestSnapshotRefusesAThickVolume(t *testing.T) {
	r := newRig(t)
	d := desired()
	d.Thin = false
	reconcile(t, r, d)
	err := r.rt.Snapshot(context.Background(), "vol-a1", "s")
	if experrors.KindOf(err) != experrors.KindInvalid {
		t.Errorf("kind %v, want invalid: %v", experrors.KindOf(err), err)
	}
}

func TestSnapshotRefusesAnInvalidNameAndAnAbsentVolume(t *testing.T) {
	r, _ := snapRig(t)
	if err := r.rt.Snapshot(context.Background(), "vol-a1", "Bad Name"); experrors.KindOf(err) != experrors.KindInvalid {
		t.Errorf("bad name: kind %v, want invalid", experrors.KindOf(err))
	}
	if err := r.rt.Snapshot(context.Background(), "vol-zz", "s"); experrors.KindOf(err) != experrors.KindNotFound {
		t.Errorf("absent volume: kind %v, want not found", experrors.KindOf(err))
	}
}

func TestSnapshotsListsOnlyThisVolumesSnapshotsByName(t *testing.T) {
	r, _ := snapRig(t)
	r.lvm.lvs["vol-b2"] = lvm.LV{Name: "vol-b2", Type: lvm.Thin}
	r.lvm.lvs["vol-b2-snap-other"] = lvm.LV{Name: "vol-b2-snap-other", Type: lvm.Thin, Origin: "vol-b2"}
	for _, s := range []string{"b", "a"} {
		if err := r.rt.Snapshot(context.Background(), "vol-a1", s); err != nil {
			t.Fatal(err)
		}
	}
	got, err := r.rt.Snapshots(context.Background(), "vol-a1")
	if err != nil || !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("Snapshots = %v, %v; want [a b]", got, err)
	}
}

func TestRestoreCopiesTheSnapshotOverTheDRBDDevice(t *testing.T) {
	r, c := snapRig(t)
	if err := r.rt.Snapshot(context.Background(), "vol-a1", "before"); err != nil {
		t.Fatal(err)
	}
	if err := r.rt.Restore(context.Background(), desired(), "before"); err != nil {
		t.Fatal(err)
	}
	want := []copyCall{{"/dev/vg0/vol-a1-snap-before", "/dev/drbd3", 64 << 20}}
	if !reflect.DeepEqual(c.calls, want) {
		t.Errorf("copies %+v, want %+v", c.calls, want)
	}
	if !r.j.has("lvm.activate vol-a1-snap-before") {
		t.Error("the snapshot was not activated before it was read")
	}
}

func TestRestoreOfAVolumeThatGrewCopiesOnlyWhatTheSnapshotHolds(t *testing.T) {
	r, c := snapRig(t)
	if err := r.rt.Snapshot(context.Background(), "vol-a1", "small"); err != nil {
		t.Fatal(err)
	}
	d := desired()
	d.SizeBytes = 256 << 20
	reconcile(t, r, d)
	if err := r.rt.Restore(context.Background(), d, "small"); err != nil {
		t.Fatal(err)
	}
	if got, cap := c.calls[0].n, r.lvm.lvs["vol-a1-snap-small"].Size; got != cap || got >= d.SizeBytes {
		t.Errorf("copied %d bytes; want the snapshot's %d, below the grown %d", got, cap, d.SizeBytes)
	}
}

func TestRestoreWritesNothingUnlessThisNodeIsThePrimary(t *testing.T) {
	for _, role := range []drbd.Role{drbd.RoleSecondary, drbd.RoleUnknown} {
		r, c := snapRig(t)
		if err := r.rt.Snapshot(context.Background(), "vol-a1", "s"); err != nil {
			t.Fatal(err)
		}
		r.drbd.role = role
		err := r.rt.Restore(context.Background(), desired(), "s")
		if experrors.KindOf(err) != experrors.KindConflict || len(c.calls) != 0 {
			t.Errorf("role %s: kind %v, %d copies; want conflict and none", role, experrors.KindOf(err), len(c.calls))
		}
	}
}

func TestRestoreOfAnUnknownSnapshotWritesNothing(t *testing.T) {
	r, c := snapRig(t)
	err := r.rt.Restore(context.Background(), desired(), "nope")
	if experrors.KindOf(err) != experrors.KindNotFound || len(c.calls) != 0 {
		t.Errorf("kind %v, %d copies; want not found and none", experrors.KindOf(err), len(c.calls))
	}
}

func TestRestoreReportsACopyFailure(t *testing.T) {
	r, c := snapRig(t)
	if err := r.rt.Snapshot(context.Background(), "vol-a1", "s"); err != nil {
		t.Fatal(err)
	}
	c.err = errors.New("device busy")
	if err := r.rt.Restore(context.Background(), desired(), "s"); !errors.Is(err, c.err) {
		t.Errorf("err = %v, want the copy's error", err)
	}
}

func TestRemoveDropsTheSnapshotsBeforeTheirVolume(t *testing.T) {
	r, _ := snapRig(t)
	for _, s := range []string{"a", "b"} {
		if err := r.rt.Snapshot(context.Background(), "vol-a1", s); err != nil {
			t.Fatal(err)
		}
	}
	r.j.calls = nil
	if err := remove(t, r); err != nil {
		t.Fatal(err)
	}
	want := []string{"drbd.down", "lvm.remove vol-a1-snap-a", "lvm.remove vol-a1-snap-b", "lvm.remove vol-a1"}
	if got := r.j.mutating(); !reflect.DeepEqual(got, want) {
		t.Errorf("calls %v, want %v", got, want)
	}
	if len(r.lvm.lvs) != 0 {
		t.Errorf("LVs left behind: %v", r.lvm.lvs)
	}
}
