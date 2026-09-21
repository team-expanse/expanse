package controller

import (
	"context"
	"slices"
	"time"

	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/storage/drbd"
)

// DefaultLostAfter is how long a node must stay gone before its replicas are given up on.
const DefaultLostAfter = 10 * time.Minute

// rebuildReplicas keeps a volume at its replication factor when a node is gone for
// good. It takes one step per round, each safe to repeat after a crash:
//
//  1. bring the placement rows in line with the allocation;
//  2. retire the id of a lost replica, only when a spare node can replace it and a
//     current replica remains to copy from;
//  3. once every survivor has forgotten that id, add a spare under a fresh one.
//
// The order matters: a dead but unforgotten peer still counts toward quorum, so the
// replacement must not join before the survivors have let go of the old id.
func (c *Controller) rebuildReplicas(ctx context.Context, id string, spec *storage.Spec, meshed map[string]bool) {
	if c.opts.Alloc == nil {
		return
	}
	status, _, err := storage.LoadStatus(ctx, c.opts.St, id)
	if err != nil {
		return
	}
	lost := c.lostHosts(status.Placement, meshed)
	switch status.State {
	case storage.StateHealthy, storage.StateDegraded, storage.StateReadOnly:
	default:
		return
	}
	if c.alignPlacement(ctx, id) {
		return // a crash left the rows behind the allocation; the rest waits for a settled round
	}
	al, err := c.opts.Alloc.Get(ctx, id)
	if err != nil {
		return
	}
	if !hasHealthyReplica(status.Placement, meshed) || len(al.Retired) > 0 {
		return
	}
	spare := c.spareNode(ctx, meshed, status.Placement)
	if spare == "" {
		return
	}
	switch {
	case len(al.NodeIDs) < spec.Replication:
		c.addReplica(ctx, id, spare)
	case len(lost) > 0:
		c.retireReplica(ctx, id, lost[0])
	default:
		return
	}
	c.alignPlacement(ctx, id)
}

func (c *Controller) addReplica(ctx context.Context, id, host string) {
	nodeID, err := c.opts.Alloc.AssignNodeID(ctx, id, host)
	if err != nil {
		c.log.Warn("cannot give the replacement replica a node-id; will retry", "vol", id, "node", host, "err", err)
		return
	}
	c.log.Info("replacement replica added", "vol", id, "node", host, "nodeID", nodeID)
}

func (c *Controller) retireReplica(ctx context.Context, id, host string) {
	nodeID, err := c.opts.Alloc.RetireNode(ctx, id, host)
	if err != nil {
		c.log.Warn("cannot retire a lost replica; will retry", "vol", id, "node", host, "err", err)
		return
	}
	c.log.Warn("replica given up on: its node stayed gone", "vol", id, "node", host, "nodeID", nodeID)
}

// alignPlacement writes the placement rows to name exactly the allocation's members
// and reports whether it changed them. The allocation is the commit point: a node
// gains or loses a replica when its node-id does.
func (c *Controller) alignPlacement(ctx context.Context, id string) bool {
	al, err := c.opts.Alloc.Get(ctx, id)
	if err != nil {
		return false
	}
	status, rev, err := storage.LoadStatus(ctx, c.opts.St, id)
	if err != nil || !matchMembers(&status, al) {
		return false
	}
	if err := storage.CompareAndSwapStatus(ctx, c.opts.St, id, rev, status); err != nil {
		c.log.Warn("cannot align the placement with the allocation; will retry", "vol", id, "err", err)
	}
	return true
}

func matchMembers(status *storage.Status, al drbd.Allocation) bool {
	before := len(status.Placement)
	status.Placement = slices.DeleteFunc(status.Placement, func(r storage.Replica) bool {
		_, member := al.NodeIDs[r.NodeID]
		return !member
	})
	changed := len(status.Placement) != before
	for host := range al.NodeIDs {
		if !slices.ContainsFunc(status.Placement, func(r storage.Replica) bool { return r.NodeID == host }) {
			status.Placement = append(status.Placement, storage.Replica{NodeID: host})
			changed = true
		}
	}
	return changed
}

func hasHealthyReplica(placement []storage.Replica, meshed map[string]bool) bool {
	return slices.ContainsFunc(placement, func(r storage.Replica) bool { return r.Healthy && meshed[r.NodeID] })
}

// spareNode is the lowest-id node that can take a replica and holds none of this volume.
func (c *Controller) spareNode(ctx context.Context, meshed map[string]bool, placement []storage.Replica) string {
	holders := make([]string, len(placement))
	for i, r := range placement {
		holders[i] = r.NodeID
	}
	class := storage.DefaultStorageClass()
	class.Replication = 1
	chosen, err := storage.SelectNodes(class, c.storageNodes(ctx, meshed), holders)
	if err != nil {
		return ""
	}
	return chosen[0].ID
}

// lostHosts are the placement's nodes that have been gone for LostAfter. The clock
// starts when this controller first sees a node down, so a new leader restarts the
// wait rather than giving up on a replica early.
func (c *Controller) lostHosts(placement []storage.Replica, meshed map[string]bool) []string {
	var lost []string
	for _, r := range placement {
		if meshed[r.NodeID] {
			continue
		}
		since, seen := c.downSince[r.NodeID]
		if !seen {
			since = c.opts.Now()
			c.downSince[r.NodeID] = since
		}
		if c.opts.Now().Sub(since) >= c.opts.LostAfter {
			lost = append(lost, r.NodeID)
		}
	}
	slices.Sort(lost)
	return lost
}

// forgetLiveNodes ends the wait of every node that is back, so a later outage starts a fresh one.
func (c *Controller) forgetLiveNodes(meshed map[string]bool) {
	for host := range c.downSince {
		if meshed[host] {
			delete(c.downSince, host)
		}
	}
}
