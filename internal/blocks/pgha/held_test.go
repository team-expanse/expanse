package pgha

import (
	"context"
	"os"
	"testing"

	"github.com/expanse/expanse/internal/cluster/lease"
)

func heldMarked(mount string) bool {
	_, err := os.Stat(HeldPath(mount))
	return err == nil
}

func TestAnActivePrimaryMarksItsLeaseHeld(t *testing.T) {
	st := newTestStore(t)
	ref := "default/pg"
	mount := t.TempDir()
	c := New(Config{Leases: lease.NewManager(st, "n1"), Self: "n1", ResolveAddr: func(string) string { return "" }})
	inst := Instance{BlockRef: ref, MountPath: mount, Port: 5432}

	c.Pass(context.Background(), map[string]Instance{ref: inst})
	if !heldMarked(mount) {
		t.Fatal("an elected primary did not mark its lease held")
	}
	// The workload clears the mark before it starts; the next pass must restore it.
	if err := os.Remove(HeldPath(mount)); err != nil {
		t.Fatal(err)
	}
	c.Pass(context.Background(), map[string]Instance{ref: inst})
	if !heldMarked(mount) {
		t.Fatal("an active primary did not restore a cleared held mark")
	}
}

func TestAReclaimedPrimaryMarksItsLeaseHeld(t *testing.T) {
	st := newTestStore(t)
	ref := "default/pg"
	mount := t.TempDir()
	if err := writeRoleFile(RolePath(mount), primaryContent); err != nil {
		t.Fatal(err)
	}
	c := New(Config{Leases: lease.NewManager(st, "n1"), Self: "n1", ResolveAddr: func(string) string { return "" }})

	c.Pass(context.Background(), map[string]Instance{ref: {BlockRef: ref, MountPath: mount, Port: 5432}})
	if !heldMarked(mount) {
		t.Fatal("a reclaimed primary did not mark its lease held")
	}
}

func TestADemotedPrimaryClearsItsHeldMark(t *testing.T) {
	st := newTestStore(t)
	ref := "default/pg"
	mount := t.TempDir()
	if err := writeRoleFile(RolePath(mount), primaryContent); err != nil {
		t.Fatal(err)
	}
	if err := writeRoleFile(HeldPath(mount), "held\n"); err != nil {
		t.Fatal(err)
	}
	held, err := lease.NewManager(st, "n3").TryAcquire(context.Background(), LeaseName(ref), LeaseTTL)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Abandon()
	c := New(Config{Leases: lease.NewManager(st, "n1"), Self: "n1", ResolveAddr: func(string) string { return "10.42.0.9" }})

	c.Pass(context.Background(), map[string]Instance{ref: {BlockRef: ref, MountPath: mount, Port: 5432}})
	if heldMarked(mount) {
		t.Fatal("a primary that lost its lease to another node kept its held mark")
	}
}
