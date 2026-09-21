package controller

import (
	"context"
	"slices"

	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/store"
)

// electPrimary keeps the current primary while it is usable and otherwise elects
// the lowest-id healthy replica. Election only chooses who tries to be primary:
// DRBD refuses a promotion without current data or quorum, and the volume lease
// admits one holder, so a poor choice costs time, never data.
func (c *Controller) electPrimary(ctx context.Context, volID string, status *storage.Status, rev store.Revision, meshed map[string]bool) {
	if i := slices.IndexFunc(status.Placement, func(r storage.Replica) bool { return r.NodeID == status.Primary }); i >= 0 && usable(status.Placement[i], meshed) {
		return
	}
	next := firstCandidate(status.Placement, meshed)
	if next == "" {
		return
	}
	was := status.Primary
	status.Primary = next
	if err := storage.CompareAndSwapStatus(ctx, c.opts.St, volID, rev, *status); err != nil {
		c.log.Warn("primary election CAS failed; will retry next round", "vol", volID, "primary", next, "err", err)
		return
	}
	c.log.Info("primary elected", "vol", volID, "primary", next, "was", was)
}

// usable reports whether a replica can stay primary: its node is alive and it does
// not report being behind or without quorum. A replica that has reported nothing yet
// (a volume being created) is kept.
func usable(r storage.Replica, meshed map[string]bool) bool {
	return meshed[r.NodeID] && (r.Role == "" || r.Healthy)
}

// firstCandidate is the lowest-id live replica that reports current data and quorum.
func firstCandidate(placement []storage.Replica, meshed map[string]bool) string {
	best := ""
	for _, r := range placement {
		if meshed[r.NodeID] && r.Healthy && (best == "" || r.NodeID < best) {
			best = r.NodeID
		}
	}
	return best
}
