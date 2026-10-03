package controller

import (
	"context"

	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/store"
)

// writeQuorum is how many healthy replicas keep a volume writable. DRBD runs with
// quorum off below three members, so a lone survivor keeps writing; from three up
// it needs a majority.
func writeQuorum(members int) int {
	if members < 3 {
		return 1
	}
	return members/2 + 1
}

// vote is a diskless tiebreaker's say in DRBD quorum.
type vote int

const (
	noTiebreaker vote = iota
	tiebreakerDown
	tiebreakerUp
)

// derivedState is the volume state its healthy count implies, given its current
// members and target. Quorum follows the members and the tiebreaker, as DRBD's does.
func derivedState(healthy, members, target int, tb vote) storage.VolumeState {
	voters, votes := members, healthy
	if tb != noTiebreaker {
		voters++
	}
	if tb == tiebreakerUp {
		votes++
	}
	switch {
	case votes < writeQuorum(voters):
		return storage.StateReadOnly
	case healthy < members:
		return storage.StateDegraded
	case members < target:
		return storage.StateUnderReplicated
	}
	return storage.StateHealthy
}

// enforceReplication keeps the volume's State in step with the replicas that are
// healthy and reachable, alerting while it is short. A volume still Creating only
// leaves that state once every replica is healthy.
func (c *Controller) enforceReplication(ctx context.Context, volID string, spec *storage.Spec, status *storage.Status, rev store.Revision, meshed map[string]bool) {
	healthy := 0
	for _, p := range status.Placement {
		if p.Healthy && meshed[p.NodeID] {
			healthy++
		}
	}
	want := derivedState(healthy, len(status.Placement), spec.Replication, c.tiebreakerVote(ctx, volID, meshed))
	if c.shouldAlert(ctx, want, status, meshed) {
		c.emitAlert(AlertEvent{
			VolID: volID, Kind: "under-replication", Have: healthy, Want: spec.Replication,
			Detail: "replication factor cannot be met",
		})
	}
	switch status.State {
	case storage.StateHealthy, storage.StateDegraded, storage.StateReadOnly, storage.StateUnderReplicated:
	case storage.StateCreating:
		if want != storage.StateHealthy && want != storage.StateUnderReplicated {
			return
		}
	default:
		return
	}
	if status.State != want {
		status.State = want
		_ = storage.CompareAndSwapStatus(ctx, c.opts.St, volID, rev, *status)
	}
}

// shouldAlert: a short volume alerts once out of Creating, except when it is short
// only by design and no spare node exists to grow it.
func (c *Controller) shouldAlert(ctx context.Context, want storage.VolumeState, status *storage.Status, meshed map[string]bool) bool {
	switch {
	case want == storage.StateHealthy:
		return false
	case want == storage.StateUnderReplicated:
		return c.spareNode(ctx, meshed, status.Placement) != ""
	}
	return status.State != storage.StateCreating
}
