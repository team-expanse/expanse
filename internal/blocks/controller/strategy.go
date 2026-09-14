package controller

// Strategy-specific placement (Phase 04 T16): singleton lease fencing
// (spec §4.4, G4.11) and daemonset per-node placement.

import (
	"context"

	"github.com/expanse/expanse/internal/cluster/lease"
	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/scheduler"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
)

// singletonLeaseName is the lease name for a singleton block. Lease keys
// live under the manager's /leases/ prefix, so the full store key is
// /leases/block/<ns>/<name> — the card's "/blocks/<ns>/<name>/singleton"
// lease, namespaced into the lease tree (documented deviation).
func singletonLeaseName(ns, name string) string {
	return "block/" + ns + "/" + name
}

// leaseManager lazily builds the lease manager for this controller,
// bound to the raftstore's node ID. The manager itself is created once
// and reused; each Held renews itself in the background.
func (c *Controller) leaseManager() *lease.Manager {
	c.leaseOnce.Do(func() {
		c.leases = lease.NewManager(c.St, c.St.NodeID())
	})
	return c.leases
}

// acquireSingleton ensures this controller holds the block's singleton
// lease before any placement is scheduled. The lease — not the replica
// count — is what enforces "exactly one instance cluster-wide, ever"
// (G4.11): a healed partition cannot produce two placements because the
// second would-be placer's CAS through the Raft-replicated lease key
// fails while the first holder's lease is live.
func (c *Controller) acquireSingleton(ctx context.Context, ns, name string) error {
	key := singletonLeaseName(ns, name)

	c.leaseMu.Lock()
	held := c.held[key]
	c.leaseMu.Unlock()
	if held != nil && held.Valid() {
		return nil
	}

	mgr := c.leaseManager()
	h, err := mgr.TryAcquire(ctx, key, lease.DefaultTTL)
	if err == nil {
		c.leaseMu.Lock()
		c.held[key] = h
		c.leaseMu.Unlock()
		return nil
	}
	if errors.Is(err, errors.KindConflict) {
		// Held live by another placer (e.g. a pre-heal leader's
		// placement whose lease has not expired). Not an error — a
		// pending state to retry on the next pass.
		return err
	}
	return errors.Wrap(err, errors.KindUnavailable, "controller.acquireSingleton", key)
}

// placeSingleton places the single replica of a singleton block, gated
// on the lease.
func (c *Controller) placeSingleton(ctx context.Context, b *pb.Block, e store.Entry, status *pb.BlockStatus, nodes []scheduler.NodeView, cfg scheduler.OvercommitConfig) (int, error) {
	if err := c.acquireSingleton(ctx, b.GetMetadata().GetNamespace(), b.GetMetadata().GetName()); err != nil {
		if errors.Is(err, errors.KindConflict) {
			status.Phase = pb.Phase_PENDING
			status.PendingReason = &pb.PendingReason{Code: "LeaseHeld", Message: "singleton lease held elsewhere"}
			if err := c.persistStatus(ctx, e.Key, status); err != nil {
				return 0, err
			}
			return 0, nil
		}
		return 0, err
	}
	return c.placeReplicas(ctx, b, e, status, nodes, cfg)
}

// placeReplicas is the generic scheduler loop for index-addressed
// strategies (active-active, primary-replica, singleton).
func (c *Controller) placeReplicas(ctx context.Context, b *pb.Block, e store.Entry, status *pb.BlockStatus, nodes []scheduler.NodeView, cfg scheduler.OvercommitConfig) (int, error) {
	want := int(b.GetSpec().GetReplicas())
	var existing []string
	for _, p := range status.GetPlacements() {
		if p.GetReplicaIndex() != -1 {
			existing = append(existing, p.GetNodeId())
		}
	}

	placed := 0
	for i := 0; i < want; i++ {
		if placementAt(status, int32(i)) != nil {
			continue
		}
		nodeID, pending := scheduler.Schedule(nodes, scheduler.ReplicaRequest{
			Block:              b,
			ReplicaIndex:       i,
			ExistingPlacements: existing,
		}, cfg, scheduler.ClusterView{SameBlockReplicas: sameBlockReplicas(existing)})
		if pending != nil {
			// Persist the reason but do NOT advance the phase (§4.3):
			// the replica stays Pending and is retried on the next trigger.
			status.Phase = pb.Phase_PENDING
			status.PendingReason = pending
			break
		}
		status.PendingReason = nil
		status.Phase = pb.Phase_SCHEDULING
		status.Placements = append(status.Placements, &pb.PlacementStatus{
			ReplicaIndex: int32(i),
			NodeId:       nodeID,
			Phase:        pb.Phase_SCHEDULING,
			Generation:   int64(e.Revision),
		})
		existing = append(existing, nodeID)
		placed++
	}
	if placed > 0 || status.GetPendingReason() != nil {
		if err := c.persistStatus(ctx, e.Key, status); err != nil {
			return placed, err
		}
	}
	return placed, nil
}

// placeDaemonset keeps exactly one placement per eligible node: Ready,
// non-witness (G4.11 daemonset clause). Cordon is ignored by default
// (spec §4.4: daemonsets place regardless of drain state unless the
// caller drains via Phase 05). New nodes auto-extend on the next pass;
// removed nodes are culled, with the runtime stop hook fired per cull
// (Phase 05 wires the real stop; the hook defaults to a no-op).
func (c *Controller) placeDaemonset(ctx context.Context, b *pb.Block, e store.Entry, status *pb.BlockStatus, nodes []scheduler.NodeView) (int, error) {
	desired := map[string]bool{}
	for _, n := range nodes {
		if n.Ready && !n.Witness {
			desired[n.ID] = true
		}
	}

	changed := false
	placed := 0

	// Cull placements on nodes no longer eligible.
	kept := status.Placements[:0]
	for _, p := range status.GetPlacements() {
		if desired[p.GetNodeId()] {
			kept = append(kept, p)
			continue
		}
		if err := c.hooks().stop(ctx, b, p); err != nil {
			// Do not drop the record until the runtime confirms stop.
			kept = append(kept, p)
			continue
		}
		changed = true
	}
	status.Placements = kept

	// Fill placements for new eligible nodes.
	for _, n := range nodes {
		if !desired[n.ID] || placementOn(status, n.ID) != nil {
			continue
		}
		idx := int32(len(status.GetPlacements()))
		for placementAt(status, idx) != nil {
			idx++
		}
		status.Placements = append(status.Placements, &pb.PlacementStatus{
			ReplicaIndex: idx,
			NodeId:       n.ID,
			Phase:        pb.Phase_SCHEDULING,
			Generation:   int64(e.Revision),
		})
		changed = true
		placed++
	}

	if changed {
		status.Phase = pb.Phase_SCHEDULING
		status.PendingReason = nil
		if err := c.persistStatus(ctx, e.Key, status); err != nil {
			return placed, err
		}
	}
	return placed, nil
}

// placementOn finds the placement record on a given node.
func placementOn(st *pb.BlockStatus, nodeID string) *pb.PlacementStatus {
	for _, p := range st.GetPlacements() {
		if p.GetNodeId() == nodeID {
			return p
		}
	}
	return nil
}

// persistStatus marshals and writes the status record.
func (c *Controller) persistStatus(ctx context.Context, k store.Key, status *pb.BlockStatus) error {
	out, err := proto.Marshal(status)
	if err != nil {
		return errors.Wrap(err, errors.KindInternal, "controller.persistStatus", "marshal status")
	}
	if _, err := c.St.Txn(ctx, []store.Op{
		{Kind: store.OpPut, Key: statusKey(k), Value: out},
	}); err != nil {
		return errors.Wrap(err, errors.KindUnavailable, "controller.persistStatus", string(k))
	}
	return nil
}
