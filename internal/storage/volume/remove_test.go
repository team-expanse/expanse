package volume

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	experrors "github.com/expanse/expanse/internal/errors"
)

func (f *fakeDRBD) Down(context.Context, string) error {
	f.j.add("drbd.down")
	if f.downErr != nil {
		return f.downErr
	}
	f.up = false
	return nil
}

func (f *fakeLVM) Remove(_ context.Context, vg, name string) error {
	f.j.add("lvm.remove %s", name)
	if f.removeErr != nil {
		return f.removeErr
	}
	delete(f.lvs, name)
	return nil
}

func remove(t *testing.T, r *rig) error {
	t.Helper()
	return r.rt.Remove(context.Background(), "vol-a1")
}

func configExists(r *rig) bool {
	_, err := os.Stat(filepath.Join(r.dir, "vol-a1.res"))
	return err == nil
}

func TestRemoveTakesDownThenDropsConfigThenTheLV(t *testing.T) {
	r := newRig(t)
	reconcile(t, r, desired())
	r.j.calls = nil
	if err := remove(t, r); err != nil {
		t.Fatal(err)
	}
	if want := []string{"drbd.down", "lvm.remove vol-a1"}; !reflect.DeepEqual(r.j.mutating(), want) {
		t.Errorf("calls %v, want %v", r.j.mutating(), want)
	}
	if configExists(r) {
		t.Error("config file survived")
	}
	if _, ok := r.lvm.lvs["vol-a1"]; ok {
		t.Error("backing LV survived")
	}
}

func TestRemoveOfAnAbsentVolumeChangesNothing(t *testing.T) {
	r := newRig(t)
	if err := remove(t, r); err != nil {
		t.Fatal(err)
	}
	if m := r.j.mutating(); len(m) != 0 {
		t.Errorf("acted on nothing: %v", m)
	}
}

func TestRemoveTwiceIsSafe(t *testing.T) {
	r := newRig(t)
	reconcile(t, r, desired())
	for range 2 {
		if err := remove(t, r); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRemoveKeepsConfigAndLVWhenDownFails(t *testing.T) {
	r := newRig(t)
	reconcile(t, r, desired())
	r.drbd.downErr = errors.New("device is open")
	if err := remove(t, r); err == nil {
		t.Fatal("expected the down failure")
	}
	if !configExists(r) || r.lvm.lvs["vol-a1"].Name == "" {
		t.Error("teardown went on past a failed down")
	}
}

func TestRemoveResumesAfterAFailedLVRemove(t *testing.T) {
	r := newRig(t)
	reconcile(t, r, desired())
	r.lvm.removeErr = experrors.New(experrors.KindUnavailable, "fake", "busy")
	if err := remove(t, r); err == nil {
		t.Fatal("expected the lvremove failure")
	}
	r.lvm.removeErr = nil
	if err := remove(t, r); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.lvm.lvs["vol-a1"]; ok {
		t.Error("LV not removed on the retry")
	}
}

func TestPresentSeesTheBackingLV(t *testing.T) {
	r := newRig(t)
	if ok, err := r.rt.Present(context.Background(), "vol-a1"); err != nil || ok {
		t.Fatalf("before: %v, %v", ok, err)
	}
	reconcile(t, r, desired())
	if ok, err := r.rt.Present(context.Background(), "vol-a1"); err != nil || !ok {
		t.Fatalf("after: %v, %v", ok, err)
	}
}
