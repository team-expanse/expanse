package local

import (
	"context"
	"os"
	"testing"

	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/storage"
)

func newDriver(t *testing.T) *Driver {
	t.Helper()
	return New(t.TempDir() + "/local-vols")
}

func testVol(size uint64) *storage.Volume {
	return &storage.Volume{ID: "vol-1a2b3c4d5e6f7a8b", SizeBytes: size, Class: "local"}
}

func TestCreateAttachDelete(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)
	v := testVol(1 << 20)

	if err := d.Create(ctx, v); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Sparse file: created at full logical size with on-disk size ~0.
	st, err := os.Stat(d.path(v.ID))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Size() != 1<<20 {
		t.Errorf("size = %d, want %d", st.Size(), 1<<20)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("perm = %v, want 0600", st.Mode().Perm())
	}

	p, err := d.Attach(ctx, v.ID, "n1")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if p != d.path(v.ID) {
		t.Errorf("Attach path = %q, want %q", p, d.path(v.ID))
	}

	if err := d.Delete(ctx, v.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(d.path(v.ID)); !os.IsNotExist(err) {
		t.Errorf("file should be gone, got err %v", err)
	}
}

func TestCreateDuplicate(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)
	v := testVol(1 << 20)
	if err := d.Create(ctx, v); err != nil {
		t.Fatalf("Create: %v", err)
	}
	err := d.Create(ctx, v)
	if experrors.KindOf(err) != experrors.KindConflict {
		t.Errorf("KindOf = %v, want conflict", experrors.KindOf(err))
	}
}

func TestCreateInvalidID(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)
	err := d.Create(ctx, &storage.Volume{})
	if experrors.KindOf(err) != experrors.KindInvalid {
		t.Errorf("KindOf = %v, want invalid", experrors.KindOf(err))
	}
}

func TestDeleteAttachStatusNotFound(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"Delete", d.Delete(ctx, "vol-missing")},
		{"Status", func() error { _, err := d.Status(ctx, "vol-missing"); return err }()},
	} {
		if experrors.KindOf(tc.err) != experrors.KindNotFound {
			t.Errorf("%s KindOf = %v, want not_found", tc.name, experrors.KindOf(tc.err))
		}
	}
	_, err := d.Attach(ctx, "vol-missing", "n1")
	if experrors.KindOf(err) != experrors.KindNotFound {
		t.Errorf("Attach KindOf = %v, want not_found", experrors.KindOf(err))
	}
}

func TestResizeGrowOnly(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)
	v := testVol(1 << 20)
	if err := d.Create(ctx, v); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := d.Resize(ctx, v.ID, 4<<20); err != nil {
		t.Fatalf("Resize grow: %v", err)
	}
	st, _ := os.Stat(d.path(v.ID))
	if st.Size() != 4<<20 {
		t.Errorf("size = %d, want %d", st.Size(), 4<<20)
	}
	err := d.Resize(ctx, v.ID, 1<<20)
	if experrors.KindOf(err) != experrors.KindInvalid {
		t.Errorf("shrink KindOf = %v, want invalid", experrors.KindOf(err))
	}
}

func TestSnapshotsUnsupported(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)
	if err := d.Snapshot(ctx, "vol-x", "snap"); experrors.KindOf(err) != experrors.KindUnavailable {
		t.Errorf("Snapshot KindOf = %v, want unavailable", experrors.KindOf(err))
	}
	if err := d.RestoreSnapshot(ctx, "vol-x", "snap"); experrors.KindOf(err) != experrors.KindUnavailable {
		t.Errorf("RestoreSnapshot KindOf = %v, want unavailable", experrors.KindOf(err))
	}
	snaps, err := d.ListSnapshots(ctx, "vol-x")
	if err != nil || len(snaps) != 0 {
		t.Errorf("ListSnapshots = %v, %v; want empty, nil", snaps, err)
	}
}

func TestStatusHealthy(t *testing.T) {
	ctx := context.Background()
	d := newDriver(t)
	v := testVol(1 << 20)
	if err := d.Create(ctx, v); err != nil {
		t.Fatalf("Create: %v", err)
	}
	s, err := d.Status(ctx, v.ID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !s.Healthy {
		t.Errorf("Healthy = false, want true (%s)", s.Details)
	}
}

func TestCapabilities(t *testing.T) {
	c := newDriver(t).Capabilities()
	if c.SupportsRWX {
		t.Error("local driver must not support RWX")
	}
	if c.SupportsSnapshot {
		t.Error("local driver must not support snapshots")
	}
	if !c.SupportsOnlineResize {
		t.Error("sparse-file grow should be online")
	}
}

func TestDetachStateless(t *testing.T) {
	if err := newDriver(t).Detach(context.Background(), "vol-x", "n1"); err != nil {
		t.Errorf("Detach: %v", err)
	}
}

// Compile-time check that the local driver satisfies the §4.2 interface.
var _ storage.Driver = (*Driver)(nil)
