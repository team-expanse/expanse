// Package controller implements the leader-side placement controller
// (PHASE04.md §9 item 9, §4.3). It watches /blocks/ for blocks with
// unplaced replicas, runs scheduler.Schedule per replica, and persists
// placements.
//
// Key deviation from the task card, deliberate: placements and replica
// phases are written under /blocks/<ns>/<name>/status (proto-binary
// pb.BlockStatus), NOT /blocks/<ns>/<name>/placements. Reason: the FSM's
// generation snapshotter treats every key under /blocks/ that is not in a
// /status/ subtree as DESIRED state (generation.IsDesiredKey), so the
// card's key would bump the cluster generation on every reconcile pass —
// including the 30 s retry timer — defeating Phase03 §4.7's "status never
// creates generations" rule. The /status/ subtree is exactly the escape
// hatch that rule provides; controller writes are observed state.
//
// Retry triggers (§4.3): node join, node uncordon, resource release,
// block delete, or every 30 s. The first four surface as Notify* methods
// that wake the run loop; real membership/nodelc events adapt to them
// with one-line callbacks (see MembershipHooks).
package controller

import (
	"context"
	"sync"
	"time"

	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/scheduler"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/raftstore"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
)

// DefaultInterval is the §4.3 retry timer.
const DefaultInterval = 30 * time.Second

// StatusSuffix appends to a block key for its observed-state record.
const StatusSuffix = "/status"

// Controller is the leader-only placement loop. Nodes supplies the
// simulated-or-real cluster view; in production it reads node status
// entries (Phase 05 wires the real adapter), in tests it returns a fixed
// NodeView slice.
type Controller struct {
	St    *raftstore.Store
	Nodes func(ctx context.Context) ([]scheduler.NodeView, scheduler.OvercommitConfig, error)
	// Interval between passes; DefaultInterval when zero.
	Interval time.Duration
	// Update carries the §5.2 rolling-update seams; nil fields get
	// permissive defaults (see UpdateHooks).
	Update *UpdateHooks

	mu    sync.Mutex
	wake  chan struct{}
	calls map[string]int // notify kind -> count (test observability)
}

// New builds a Controller.
func New(st *raftstore.Store, nodes func(context.Context) ([]scheduler.NodeView, scheduler.OvercommitConfig, error)) *Controller {
	return &Controller{St: st, Nodes: nodes, wake: make(chan struct{}, 1), calls: map[string]int{}}
}

// hooks returns the update hooks, defaulting to an empty set.
func (c *Controller) hooks() *UpdateHooks {
	if c.Update == nil {
		return &UpdateHooks{}
	}
	return c.Update
}

// Run drives reconcile passes until ctx is done: on every Interval tick
// and on every Notify* trigger. Follower nodes skip work inside Reconcile.
func (c *Controller) Run(ctx context.Context) {
	ival := c.Interval
	if ival <= 0 {
		ival = DefaultInterval
	}
	t := time.NewTicker(ival)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.Reconcile(ctx)
		case <-c.wake:
			c.Reconcile(ctx)
		}
	}
}

func (c *Controller) notify(kind string) {
	c.mu.Lock()
	c.calls[kind]++
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default: // a pass is already pending
	}
}

// NotifyNodeJoin re-runs placement after a node joins (§4.3).
func (c *Controller) NotifyNodeJoin(nodeID string) { c.notify("node-join:" + nodeID) }

// NotifyUncordon re-runs placement after a node is uncordoned (§4.3).
func (c *Controller) NotifyUncordon(nodeID string) { c.notify("uncordon:" + nodeID) }

// NotifyBlockDelete re-runs placement after a block is deleted, freeing
// its reservations (§4.3).
func (c *Controller) NotifyBlockDelete(namespace, name string) {
	c.notify("block-delete:" + namespace + "/" + name)
}

// NotifyResourceRelease re-runs placement after resources are freed on a
// node (§4.3).
func (c *Controller) NotifyResourceRelease(nodeID string) { c.notify("resource-release:" + nodeID) }

// NotifyCount reports how many times a notify kind fired (tests).
func (c *Controller) NotifyCount(kind string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[kind]
}

// blockKey is the desired-state key for a block.
func blockKey(ns, name string) store.Key {
	return store.Key("/blocks/" + ns + "/" + name)
}

// statusKey is the observed-state key holding the pb.BlockStatus.
func statusKey(k store.Key) store.Key {
	return store.Key(string(k) + StatusSuffix)
}

// Reconcile performs one pass. Returns the number of replicas it placed.
// Followers and non-leaders are no-ops.
func (c *Controller) Reconcile(ctx context.Context) (int, error) {
	if !c.St.IsLeader() {
		return 0, nil
	}
	nodes, cfg, err := c.Nodes(ctx)
	if err != nil {
		return 0, errors.Wrap(err, errors.KindUnavailable, "controller.Reconcile", "node view")
	}
	entries, err := c.St.List(ctx, "/blocks/")
	if err != nil {
		return 0, errors.Wrap(err, errors.KindInternal, "controller.Reconcile", "list blocks")
	}
	placed := 0
	for _, e := range entries {
		// List's literal prefix includes our own /status subtrees; skip them.
		if len(string(e.Key)) > 0 && string(e.Key[len(e.Key)-len(StatusSuffix):]) == StatusSuffix {
			continue
		}
		n, err := c.placeBlock(ctx, *e, nodes, cfg)
		if err != nil {
			return placed, err
		}
		placed += n
	}
	return placed, nil
}

// placeBlock schedules every unplaced replica of one block and persists
// the merged status record.
func (c *Controller) placeBlock(ctx context.Context, e store.Entry, nodes []scheduler.NodeView, cfg scheduler.OvercommitConfig) (int, error) {
	var b pb.Block
	if err := proto.Unmarshal(e.Value, &b); err != nil {
		return 0, errors.Wrap(err, errors.KindInternal, "controller.placeBlock", "unmarshal "+string(e.Key))
	}
	want := replicaCount(&b)
	if want == 0 {
		return 0, nil
	}

	status := c.loadStatus(ctx, e.Key)
	// §5.2 rolling update: when the block is running with placements and
	// any replica is not at the current revision, run one update step
	// instead of plain placement. Generation 0 = pre-T15 placement, kept
	// at-target so old records do not trigger surprise rolls.
	target := int64(e.Revision)
	if want > 0 && len(status.GetPlacements()) > 0 &&
		b.GetSpec().GetStrategy().GetKind() != pb.StrategyKind_DAEMONSET &&
		(updateNeededLegacy(status, target) || status.GetPhase() == pb.Phase_UPDATING) {
		changed, err := c.updatePass(ctx, &b, status, target, want, nodes, cfg)
		if err != nil {
			return 0, err
		}
		if changed {
			out, err := proto.Marshal(status)
			if err != nil {
				return 0, errors.Wrap(err, errors.KindInternal, "controller.placeBlock", "marshal status")
			}
			if _, err := c.St.Txn(ctx, []store.Op{
				{Kind: store.OpPut, Key: statusKey(e.Key), Value: out},
			}); err != nil {
				return 0, errors.Wrap(err, errors.KindUnavailable, "controller.placeBlock", "persist update step")
			}
		}
		return 0, nil
	}
	// Existing placements feed P9 anti-affinity.
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
			Block:              &b,
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
		out, err := proto.Marshal(status)
		if err != nil {
			return placed, errors.Wrap(err, errors.KindInternal, "controller.placeBlock", "marshal status")
		}
		if _, err := c.St.Txn(ctx, []store.Op{
			{Kind: store.OpPut, Key: statusKey(e.Key), Value: out},
		}); err != nil {
			return placed, errors.Wrap(err, errors.KindUnavailable, "controller.placeBlock", "persist placements")
		}
	}
	return placed, nil
}

// loadStatus reads (or seeds) the status record for a block.
func (c *Controller) loadStatus(ctx context.Context, k store.Key) *pb.BlockStatus {
	e, err := c.St.Get(ctx, statusKey(k))
	if err != nil {
		return &pb.BlockStatus{Phase: pb.Phase_PENDING}
	}
	var st pb.BlockStatus
	if err := proto.Unmarshal(e.Value, &st); err != nil {
		return &pb.BlockStatus{Phase: pb.Phase_PENDING}
	}
	return &st
}

// placementAt finds an existing placement record for replica i.
func placementAt(st *pb.BlockStatus, i int32) *pb.PlacementStatus {
	for _, p := range st.GetPlacements() {
		if p.GetReplicaIndex() == i {
			return p
		}
	}
	return nil
}

// replicaCount resolves the desired replica count for scheduling:
// spec.replicas, with singleton → 1 (V5). Daemonset placement is
// per-node and lands with the daemonset lifecycle card; skipped here.
func replicaCount(b *pb.Block) int {
	switch b.GetSpec().GetStrategy().GetKind() {
	case pb.StrategyKind_SINGLETON:
		return 1
	case pb.StrategyKind_DAEMONSET:
		return 0
	}
	return int(b.GetSpec().GetReplicas())
}

// sameBlockReplicas converts existing node IDs to the ClusterView map S2
// scores against.
func sameBlockReplicas(existing []string) map[string]int {
	m := map[string]int{}
	for _, id := range existing {
		m[id]++
	}
	return m
}
