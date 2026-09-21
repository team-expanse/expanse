//go:build lvmvm

// Runs the wrapper against real LVM on scratch disks. Only the vol-lvm VM test
// builds and runs it (as root): LVM_TEST_PV and LVM_TEST_PV2 name the disks.
package lvm

import (
	"bytes"
	"crypto/rand"
	"os"
	"testing"

	experrors "github.com/expanse/expanse/internal/errors"
)

const (
	mib  = 1 << 20
	vg   = "vgtest"
	pool = "pool"
)

func envDisk(t *testing.T, name string) string {
	t.Helper()
	dev := os.Getenv(name)
	if dev == "" {
		t.Fatalf("%s must name a scratch disk", name)
	}
	return dev
}

func wantKind(t *testing.T, err error, kind experrors.Kind) {
	t.Helper()
	if experrors.KindOf(err) != kind {
		t.Errorf("want kind %v, got %v", kind, err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func mustGet(t *testing.T, e *Exec, name string) LV {
	t.Helper()
	lv, err := e.Get(ctx, vg, name)
	must(t, err)
	return lv
}

func writeAt(t *testing.T, dev string, data []byte) {
	t.Helper()
	f, err := os.OpenFile(dev, os.O_WRONLY, 0)
	must(t, err)
	defer f.Close()
	_, err = f.Write(data)
	must(t, err)
	must(t, f.Sync())
}

func readHead(t *testing.T, dev string, n int) []byte {
	t.Helper()
	f, err := os.Open(dev)
	must(t, err)
	defer f.Close()
	buf := make([]byte, n)
	_, err = f.ReadAt(buf, 0)
	must(t, err)
	return buf
}

func TestRealLVM(t *testing.T) {
	e := New()
	pv1, pv2 := envDisk(t, "LVM_TEST_PV"), envDisk(t, "LVM_TEST_PV2")

	t.Run("missing group is not found", func(t *testing.T) {
		_, err := e.VG(ctx, vg)
		wantKind(t, err, experrors.KindNotFound)
		_, err = e.List(ctx, vg)
		wantKind(t, err, experrors.KindNotFound)
	})

	t.Run("create group", func(t *testing.T) {
		must(t, e.CreateVG(ctx, vg, pv1))
		got, err := e.VG(ctx, vg)
		must(t, err)
		if got.Name != vg || got.Size < 900*mib || got.Free != got.Size {
			t.Errorf("unexpected new group %+v", got)
		}
	})

	t.Run("a second vgcreate on a used disk is refused and harms nothing", func(t *testing.T) {
		before, _ := e.VG(ctx, vg)
		if err := e.CreateVG(ctx, vg, pv1); err == nil {
			t.Error("vgcreate over an existing group succeeded")
		}
		if err := e.CreateVG(ctx, "vgother", pv1); err == nil {
			t.Error("vgcreate of a second group on a used disk succeeded")
		}
		if after, err := e.VG(ctx, vg); err != nil || after != before {
			t.Errorf("group changed: %+v -> %+v, %v", before, after, err)
		}
	})

	t.Run("thin pool", func(t *testing.T) {
		must(t, e.CreateThinPool(ctx, vg, pool, 512*mib))
		p := mustGet(t, e, pool)
		if p.Type != ThinPool || p.Size != 512*mib || p.DataPercent != 0 || !p.Active {
			t.Errorf("unexpected pool %+v", p)
		}
		wantKind(t, e.CreateThinPool(ctx, vg, pool, 512*mib), experrors.KindConflict)
	})

	t.Run("thin volume and usage", func(t *testing.T) {
		must(t, e.CreateThin(ctx, vg, pool, "thin1", 256*mib))
		v := mustGet(t, e, "thin1")
		if v.Type != Thin || v.Pool != pool || v.Size != 256*mib || v.Origin != "" || !v.Active {
			t.Errorf("unexpected thin volume %+v", v)
		}
		writeAt(t, DevicePath(vg, "thin1"), bytes.Repeat([]byte{0xAB}, 64*mib))
		p := mustGet(t, e, pool)
		if p.DataPercent < 5 || p.DataPercent > 25 || p.MetaPercent <= 0 {
			t.Errorf("64 MiB in a 512 MiB pool reads as data %.2f%% meta %.2f%%", p.DataPercent, p.MetaPercent)
		}
	})

	t.Run("thick volume", func(t *testing.T) {
		must(t, e.CreateThick(ctx, vg, "thick1", 64*mib))
		v := mustGet(t, e, "thick1")
		if v.Type != Thick || v.Size != 64*mib || v.Pool != "" || v.DataPercent != 0 || !v.Active {
			t.Errorf("unexpected thick volume %+v", v)
		}
	})

	t.Run("extend", func(t *testing.T) {
		must(t, e.Extend(ctx, vg, "thin1", 384*mib))
		if got := mustGet(t, e, "thin1").Size; got != 384*mib {
			t.Errorf("size %d after extend", got)
		}
		must(t, e.Extend(ctx, vg, "thin1", 384*mib)) // same size is a no-op
		wantKind(t, e.Extend(ctx, vg, "thin1", 128*mib), experrors.KindInvalid)
		if got := mustGet(t, e, "thin1").Size; got != 384*mib {
			t.Errorf("a refused shrink changed the size to %d", got)
		}
	})

	t.Run("snapshot keeps the data it was taken with", func(t *testing.T) {
		want := readHead(t, DevicePath(vg, "thin1"), 4096)
		must(t, e.Snapshot(ctx, vg, "thin1", "snap1"))
		s := mustGet(t, e, "snap1")
		if s.Type != Thin || s.Origin != "thin1" || s.Pool != pool || s.Active {
			t.Errorf("unexpected new snapshot %+v", s)
		}
		must(t, e.Activate(ctx, vg, "snap1"))
		if !mustGet(t, e, "snap1").Active {
			t.Error("snapshot not active after Activate")
		}
		fresh := make([]byte, 4096)
		rand.Read(fresh)
		writeAt(t, DevicePath(vg, "thin1"), fresh)
		if got := readHead(t, DevicePath(vg, "snap1"), 4096); !bytes.Equal(got, want) {
			t.Error("snapshot saw a write made after it was taken")
		}
	})

	t.Run("list shows exactly the visible volumes", func(t *testing.T) {
		lvs, err := e.List(ctx, vg)
		must(t, err)
		got := map[string]LVType{}
		for _, lv := range lvs {
			got[lv.Name] = lv.Type
		}
		want := map[string]LVType{pool: ThinPool, "thin1": Thin, "thick1": Thick, "snap1": Thin}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for name, typ := range want {
			if got[name] != typ {
				t.Errorf("%s: got type %v, want %v; all %v", name, got[name], typ, got)
			}
		}
	})

	t.Run("no free space", func(t *testing.T) {
		wantKind(t, e.CreateThick(ctx, vg, "huge", 100*1024*mib), experrors.KindResourceExhausted)
	})

	t.Run("extend the group", func(t *testing.T) {
		before, _ := e.VG(ctx, vg)
		must(t, e.ExtendVG(ctx, vg, pv2))
		after, err := e.VG(ctx, vg)
		must(t, err)
		if after.Size <= before.Size || after.Free <= before.Free {
			t.Errorf("group did not grow: %+v -> %+v", before, after)
		}
	})

	t.Run("remove", func(t *testing.T) {
		must(t, e.Remove(ctx, vg, "snap1"))
		must(t, e.Remove(ctx, vg, "thick1"))
		_, err := e.Get(ctx, vg, "snap1")
		wantKind(t, err, experrors.KindNotFound)
		wantKind(t, e.Remove(ctx, vg, "snap1"), experrors.KindNotFound)
	})
}
