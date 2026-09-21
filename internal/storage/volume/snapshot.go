package volume

import (
	"context"
	"fmt"
	"slices"
	"strings"

	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/storage/drbd"
	"github.com/expanse/expanse/internal/storage/lvm"
)

// snapLV is the LV that holds one snapshot of a volume, so a volume's snapshots
// are found by name and never confused with another volume's.
func snapLV(res, snap string) string { return res + "-snap-" + snap }

// Snapshot freezes the volume's backing LV on this node as a thin snapshot. It is
// crash-consistent, the state a power cut would leave. Repeating a name is a no-op.
func (r *Runtime) Snapshot(ctx context.Context, res, snap string) error {
	const op = "volume.Snapshot"
	if err := storage.ValidSnapshotName(snap); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	origin, err := r.LVM.Get(ctx, r.VG, res)
	if err != nil {
		return err
	}
	if origin.Type != lvm.Thin {
		return experrors.New(experrors.KindInvalid, op, fmt.Sprintf("volume %q is not thin; only thin volumes can be snapshotted", res))
	}
	name := snapLV(res, snap)
	switch have, err := r.LVM.Get(ctx, r.VG, name); {
	case err == nil && have.Origin == res:
		return nil
	case err == nil:
		return experrors.New(experrors.KindConflict, op, fmt.Sprintf("LV %q exists and is not a snapshot of %q", name, res))
	case experrors.KindOf(err) != experrors.KindNotFound:
		return err
	}
	return r.LVM.Snapshot(ctx, r.VG, res, name)
}

// Snapshots lists the snapshots this node holds for a volume, by name.
func (r *Runtime) Snapshots(ctx context.Context, res string) ([]string, error) {
	lvs, err := r.snapshotLVs(ctx, res)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(lvs))
	for i, lv := range lvs {
		names[i] = strings.TrimPrefix(lv.Name, snapLV(res, ""))
	}
	slices.Sort(names)
	return names, nil
}

func (r *Runtime) snapshotLVs(ctx context.Context, res string) ([]lvm.LV, error) {
	all, err := r.LVM.List(ctx, r.VG)
	if err != nil {
		return nil, err
	}
	var out []lvm.LV
	for _, lv := range all {
		if lv.Origin == res && strings.HasPrefix(lv.Name, snapLV(res, "")) {
			out = append(out, lv)
		}
	}
	slices.SortFunc(out, func(a, b lvm.LV) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// Restore overwrites the volume with the snapshot by writing it through the DRBD
// device, so every replica receives it and DRBD's metadata is never touched. The
// caller must have stopped every user of the device: the copy takes it exclusively.
// A volume that grew since keeps its larger size; only the snapshot's bytes are written.
func (r *Runtime) Restore(ctx context.Context, d Desired, snap string) error {
	const op = "volume.Restore"
	r.mu.Lock()
	defer r.mu.Unlock()
	st, err := r.DRBD.Status(ctx, d.Name)
	if err != nil {
		return err
	}
	if st.Role != drbd.RolePrimary {
		return experrors.New(experrors.KindConflict, op, fmt.Sprintf("volume %q is %s here; restore runs on the primary", d.Name, st.Role))
	}
	name := snapLV(d.Name, snap)
	lv, err := r.LVM.Get(ctx, r.VG, name)
	if err != nil {
		return err
	}
	if err := r.LVM.Activate(ctx, r.VG, name); err != nil {
		return err
	}
	copyFn := r.Copy
	if copyFn == nil {
		copyFn = copyDevice
	}
	return copyFn(ctx, lvm.DevicePath(r.VG, name), fmt.Sprintf("/dev/drbd%d", d.Minor), min(d.SizeBytes, lv.Size))
}

// removeSnapshots drops a volume's snapshot LVs; the origin goes after them.
func (r *Runtime) removeSnapshots(ctx context.Context, res string) error {
	lvs, err := r.snapshotLVs(ctx, res)
	if err != nil {
		return err
	}
	for _, lv := range lvs {
		if err := r.LVM.Remove(ctx, r.VG, lv.Name); err != nil {
			return err
		}
	}
	return nil
}
