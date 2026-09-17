// Package local implements the `local` storage driver (§4.2): no
// replication, a plain sparse file on the node's persistent filesystem —
// for caches and scratch data that must survive restarts but not node
// loss.
package local

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/storage"
)

// Driver stores local volumes as sparse files under root.
type Driver struct {
	Root string // e.g. /persist/expanse/local-vols
}

// New creates a driver rooted at root.
func New(root string) *Driver { return &Driver{Root: root} }

func (d *Driver) Name() string { return "local" }

func (d *Driver) path(volID string) string {
	return filepath.Join(d.Root, volID+".img")
}

func (d *Driver) Create(ctx context.Context, v *storage.Volume) error {
	if v == nil || v.ID == "" {
		return experrors.New(experrors.KindInvalid, "local.Create", "volume id required")
	}
	if err := os.MkdirAll(d.Root, 0o700); err != nil {
		return experrors.Wrap(err, experrors.KindInternal, "local.Create", "mkdir")
	}
	p := d.path(v.ID)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return experrors.New(experrors.KindConflict, "local.Create", "volume already exists: "+v.ID)
		}
		return experrors.Wrap(err, experrors.KindInternal, "local.Create", "create file")
	}
	defer f.Close()
	if err := f.Truncate(int64(v.SizeBytes)); err != nil {
		return experrors.Wrap(err, experrors.KindInternal, "local.Create", "truncate")
	}
	return nil
}

func (d *Driver) Delete(ctx context.Context, volID string) error {
	if err := os.Remove(d.path(volID)); err != nil {
		if os.IsNotExist(err) {
			return experrors.New(experrors.KindNotFound, "local.Delete", "volume not found: "+volID)
		}
		return experrors.Wrap(err, experrors.KindInternal, "local.Delete", "remove")
	}
	return nil
}

// Attach returns the backing file's path (bind-mountable as a block's
// scratch volume); the local driver has no per-node attach state.
func (d *Driver) Attach(ctx context.Context, volID, nodeID string) (string, error) {
	p := d.path(volID)
	if _, err := os.Stat(p); err != nil {
		if os.IsNotExist(err) {
			return "", experrors.New(experrors.KindNotFound, "local.Attach", "volume not found: "+volID)
		}
		return "", experrors.Wrap(err, experrors.KindInternal, "local.Attach", "stat")
	}
	return p, nil
}

func (d *Driver) Detach(ctx context.Context, volID, nodeID string) error {
	return nil // stateless attach
}

func (d *Driver) Resize(ctx context.Context, volID string, newSize uint64) error {
	p := d.path(volID)
	st, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return experrors.New(experrors.KindNotFound, "local.Resize", "volume not found: "+volID)
		}
		return experrors.Wrap(err, experrors.KindInternal, "local.Resize", "stat")
	}
	if uint64(st.Size()) > newSize {
		return experrors.New(experrors.KindInvalid, "local.Resize",
			fmt.Sprintf("shrink not supported: %d -> %d", st.Size(), newSize))
	}
	f, err := os.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		return experrors.Wrap(err, experrors.KindInternal, "local.Resize", "open")
	}
	defer f.Close()
	if err := f.Truncate(int64(newSize)); err != nil {
		return experrors.Wrap(err, experrors.KindInternal, "local.Resize", "truncate")
	}
	return nil
}

func (d *Driver) Snapshot(ctx context.Context, volID, snapName string) error {
	return experrors.New(experrors.KindUnavailable, "local.Snapshot",
		"local driver does not support snapshots")
}

func (d *Driver) RestoreSnapshot(ctx context.Context, volID, snapName string) error {
	return experrors.New(experrors.KindUnavailable, "local.RestoreSnapshot",
		"local driver does not support snapshots")
}

func (d *Driver) ListSnapshots(ctx context.Context, volID string) ([]storage.Snapshot, error) {
	return nil, nil // no snapshots, not an error
}

func (d *Driver) Status(ctx context.Context, volID string) (*storage.DriverVolumeStatus, error) {
	p := d.path(volID)
	st, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, experrors.New(experrors.KindNotFound, "local.Status", "volume not found: "+volID)
		}
		return nil, experrors.Wrap(err, experrors.KindInternal, "local.Status", "stat")
	}
	return &storage.DriverVolumeStatus{
		Healthy: true,
		Details: fmt.Sprintf("sparse file %s (%d bytes)", p, st.Size()),
	}, nil
}

func (d *Driver) Capabilities() storage.Capabilities {
	return storage.Capabilities{
		SupportsRWX:          false,
		SupportsSnapshot:     false,
		SupportsOnlineResize: true,
	}
}
