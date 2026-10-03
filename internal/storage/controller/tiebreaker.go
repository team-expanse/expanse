package controller

import (
	"context"
	"maps"
	"slices"

	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/storage/drbd"
)

// tiebreak keeps one diskless DRBD member beside a two-replica volume, so DRBD runs
// with quorum on and a partition leaves only one side writable. It takes at most one
// membership step per round, and none while a retired id is still being forgotten.
func (c *Controller) tiebreak(ctx context.Context, id string, spec *storage.Spec, meshed map[string]bool) {
	if c.opts.Alloc == nil {
		return
	}
	status, _, err := storage.LoadStatus(ctx, c.opts.St, id)
	if err != nil {
		return
	}
	switch status.State {
	case storage.StateHealthy, storage.StateDegraded, storage.StateReadOnly, storage.StateUnderReplicated:
	default:
		return
	}
	al, err := c.opts.Alloc.Get(ctx, id)
	if err != nil || len(al.Retired) > 0 {
		return
	}
	current := slices.Sorted(maps.Keys(al.Diskless))
	wanted := spec.Replication == 2 && len(al.NodeIDs) == 2
	switch {
	case len(current) > 0 && !wanted:
		c.dropTiebreaker(ctx, id, current[0], "the volume no longer has exactly two replicas")
	case len(current) > 1:
		c.dropTiebreaker(ctx, id, current[1], "one is enough")
	case len(current) == 1:
		if len(c.lostHosts(current, meshed)) > 0 && c.tiebreakerSpare(ctx, al, meshed) != "" {
			c.dropTiebreaker(ctx, id, current[0], "its node stayed gone")
		}
	case wanted && status.State == storage.StateHealthy && allLiveAndHealthy(status.Placement, meshed):
		if spare := c.tiebreakerSpare(ctx, al, meshed); spare != "" {
			c.addTiebreaker(ctx, id, spare)
		}
	}
}

// tiebreakerSpare is a node that can take the tiebreaker: one holding no part of the volume.
func (c *Controller) tiebreakerSpare(ctx context.Context, al drbd.Allocation, meshed map[string]bool) string {
	members := slices.Concat(slices.Collect(maps.Keys(al.NodeIDs)), slices.Collect(maps.Keys(al.Diskless)))
	return c.spareBeside(ctx, meshed, members)
}

func (c *Controller) addTiebreaker(ctx context.Context, id, host string) {
	nodeID, err := c.opts.Alloc.AssignDiskless(ctx, id, host)
	if err != nil {
		c.log.Warn("cannot add a tiebreaker; will retry", "vol", id, "node", host, "err", err)
		return
	}
	c.log.Info("tiebreaker added", "vol", id, "node", host, "nodeID", nodeID)
}

func (c *Controller) dropTiebreaker(ctx context.Context, id, host, why string) {
	nodeID, err := c.opts.Alloc.RetireNode(ctx, id, host)
	if err != nil {
		c.log.Warn("cannot drop a tiebreaker; will retry", "vol", id, "node", host, "err", err)
		return
	}
	c.log.Info("tiebreaker dropped", "vol", id, "node", host, "nodeID", nodeID, "why", why)
}

// allLiveAndHealthy: every replica is reachable and in sync, so quorum can turn on safely.
func allLiveAndHealthy(placement []storage.Replica, meshed map[string]bool) bool {
	return !slices.ContainsFunc(placement, func(r storage.Replica) bool { return !meshed[r.NodeID] || !r.Healthy })
}

// tiebreakerVote is how the volume's tiebreaker, if any, counts toward its quorum.
func (c *Controller) tiebreakerVote(ctx context.Context, id string, meshed map[string]bool) vote {
	if c.opts.Alloc == nil {
		return noTiebreaker
	}
	al, err := c.opts.Alloc.Get(ctx, id)
	if err != nil || len(al.Diskless) == 0 {
		return noTiebreaker
	}
	for host := range al.Diskless {
		if meshed[host] {
			return tiebreakerUp
		}
	}
	return tiebreakerDown
}
