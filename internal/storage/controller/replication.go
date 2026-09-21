package controller

import (
	"context"

	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/store"
)

// writeQuorum is how many healthy replicas keep a volume writable. DRBD runs with
// quorum off below three replicas, so a lone survivor keeps writing; from three up
// it needs a majority.
func writeQuorum(replication int) int {
	if replication < 3 {
		return 1
	}
	return replication/2 + 1
}

// derivedState is the volume state its healthy replica count implies.
func derivedState(healthy, replication int) storage.VolumeState {
	switch {
	case healthy < writeQuorum(replication):
		return storage.StateReadOnly
	case healthy < replication:
		return storage.StateDegraded
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
	want := derivedState(healthy, spec.Replication)
	if want != storage.StateHealthy && status.State != storage.StateCreating {
		c.emitAlert(AlertEvent{
			VolID: volID, Kind: "under-replication", Have: healthy, Want: spec.Replication,
			Detail: "replication factor cannot be met",
		})
	}
	switch status.State {
	case storage.StateHealthy, storage.StateDegraded, storage.StateReadOnly:
	case storage.StateCreating:
		if want != storage.StateHealthy {
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
