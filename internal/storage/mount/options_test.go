package mount

import (
	"context"
	"testing"

	"github.com/expanse/expanse/internal/storage/drbd"
)

const dirsyncSpec = `{"volId":"vol-a","mountPath":"/x","filesystem":"ext4","options":["dirsync"]}`

func TestAttachMountsWithTheRequestedOptions(t *testing.T) {
	h := newHost()
	s := spec("ext4")
	s.Options = []string{"dirsync"}
	if err := manager(h, drbd.RolePrimary).attach(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if !h.ran("mount -o noatime,dirsync " + dev) {
		t.Fatalf("want noatime,dirsync: %v", h.calls)
	}
}

func TestPlanRemountsAMountMissingAnOption(t *testing.T) {
	h := newHost()
	m := manager(h, drbd.RolePrimary)
	if err := m.attach(context.Background(), spec("ext4")); err != nil {
		t.Fatal(err)
	}
	r, err := m.Load("volume-mount:"+volID, []byte(dirsyncSpec))
	if err != nil {
		t.Fatal(err)
	}
	o, err := m.Observe(context.Background(), r)
	if err != nil || o.InSync {
		t.Fatalf("a mount without dirsync is not in sync: %+v / %v", o, err)
	}
	acts, err := m.Plan(context.Background(), r, o)
	if err != nil || len(acts) != 1 {
		t.Fatalf("want one action, got %v / %v", acts, err)
	}
	if err := m.Apply(context.Background(), acts[0]); err != nil {
		t.Fatal(err)
	}
	if !h.ran("mount -o remount,dirsync "+HostPath("", volID)) || h.ran("umount") {
		t.Fatalf("want an online remount with dirsync: %v", h.calls)
	}
	if o, _ := m.Observe(context.Background(), r); !o.InSync {
		t.Fatalf("the remounted volume is in sync: %+v", o)
	}
}

func TestLoadRefusesUnknownMountOptions(t *testing.T) {
	m := manager(newHost(), drbd.RolePrimary)
	if _, err := m.Load("x", []byte(`{"volId":"v","mountPath":"/x","options":["exec,suid"]}`)); err == nil {
		t.Fatal("only whitelisted mount options may be requested")
	}
	if _, err := m.Load("x", []byte(dirsyncSpec)); err != nil {
		t.Fatal(err)
	}
}
