package controller

import (
	"context"

	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/storage"
)

// Delete marks a volume Deleting. Each node demotes, tears down its replica and
// drops its placement row; finalizeDelete then drops the records.
func (c *Controller) Delete(ctx context.Context, volID string) error {
	status, rev, err := storage.LoadStatus(ctx, c.opts.St, volID)
	if err != nil {
		return err
	}
	status.State = storage.StateDeleting
	return storage.CompareAndSwapStatus(ctx, c.opts.St, volID, rev, status)
}

// finalizeDelete waits until every node has dropped its row, which is the proof its
// replica is gone, then releases the DRBD identity and drops the records. Releasing
// earlier would let a minor or port be reused while a replica still holds it.
func (c *Controller) finalizeDelete(ctx context.Context, volID string, spec *storage.Spec, status *storage.Status) {
	if len(status.Placement) > 0 {
		return
	}
	if c.opts.Alloc != nil {
		if err := c.opts.Alloc.Release(ctx, volID); err != nil && experrors.KindOf(err) != experrors.KindNotFound {
			c.log.Warn("cannot release the allocation of a deleted volume; will retry", "vol", volID, "err", err)
			return
		}
	}
	if err := storage.DeleteSnapshotRecords(ctx, c.opts.St, volID); err != nil {
		c.log.Warn("cannot drop the snapshot records of a deleted volume; will retry", "vol", volID, "err", err)
		return
	}
	_ = c.opts.St.Delete(ctx, storage.SpecKey(volID), 0)
	_ = c.opts.St.Delete(ctx, storage.StatusKey(volID), 0)
	c.log.Info("volume deleted", "vol", volID, "name", spec.Name)
}
