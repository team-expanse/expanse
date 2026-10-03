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
		if err := c.markTiebreakersGone(ctx, volID); err != nil {
			c.log.Warn("cannot mark the tiebreaker of a deleted volume gone; will retry", "vol", volID, "err", err)
			return
		}
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

// markTiebreakersGone tells each tiebreaker's node to take the volume down: it has no
// placement row for a deletion to wait on, and the records are about to go.
func (c *Controller) markTiebreakersGone(ctx context.Context, volID string) error {
	al, err := c.opts.Alloc.Get(ctx, volID)
	if experrors.KindOf(err) == experrors.KindNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	for host := range al.Diskless {
		if err := storage.PutGone(ctx, c.opts.St, host, volID); err != nil {
			return err
		}
	}
	return nil
}
