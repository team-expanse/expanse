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
	"log/slog"
	"sync"
	"time"

	"github.com/expanse/expanse/internal/cluster/lease"
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
	// Logger receives reconcile failures; nil = discard.
	Logger *slog.Logger
	St     *raftstore.Store
	Nodes  func(ctx context.Context) ([]scheduler.NodeView, scheduler.OvercommitConfig, error)
	// Interval between passes; DefaultInterval when zero.
	Interval time.Duration
	// Update carries the §5.2 rolling-update seams; nil fields get
	// permissive defaults (see UpdateHooks).
	Update *UpdateHooks

	mu    sync.Mutex
	wake  chan struct{}
	calls map[string]int // notify kind -> count (test observability)

	// Singleton strategy state (T16): lease manager plus the currently
	// held singleton leases, keyed by lease name.
	leaseOnce sync.Once
	leases    *lease.Manager
	leaseMu   sync.Mutex
	held      map[string]*lease.Held

	// Node-failure rescheduling (T17): §4.4 knobs and seams. Grace
	// defaults to DefaultUnreachableGrace when zero; StorageAvailable
	// defaults to true (Phase 06 wires the real volume check).
	UnreachableGrace time.Duration
	StorageAvailable func(volumeID string) bool
	unreachableMu    sync.Mutex
	unreachableSince map[string]time.Time // blockKey\x00nodeID → first unreachable
}

// New builds a Controller.
func New(st *raftstore.Store, nodes func(context.Context) ([]scheduler.NodeView, scheduler.OvercommitConfig, error)) *Controller {
	return &Controller{
		St:               st,
		Nodes:            nodes,
		wake:             make(chan struct{}, 1),
		calls:            map[string]int{},
		held:             map[string]*lease.Held{},
		unreachableSince: map[string]time.Time{},
	}
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
			if placed, err := c.Reconcile(ctx); err != nil {
				c.logReconcileErr(err)
			} else if c.Logger != nil {
				c.Logger.Info("placement pass", "placed", placed)
			}
		case <-c.wake:
			if _, err := c.Reconcile(ctx); err != nil {
				c.logReconcileErr(err)
			}
		}
	}
}

// logReconcileErr reports a failed placement pass (nil logger = discard).
func (c *Controller) logReconcileErr(err error) {
	if c.Logger != nil {
		c.Logger.Error("placement reconcile failed", "err", err)
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

// Wake nudges the run loop (production watches on /blocks/ and /nodes/
// call this; the interval ticker remains the backstop).
func (c *Controller) Wake() { c.notify("wake") }

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
	// Runtime promotion first (T20.5b): units that reached active get
	// their RUNNING phase before this pass places new replicas.
	if err := c.RuntimePass(ctx); err != nil {
		return 0, err
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
	// Daemonset has no replica count (V6): its per-node placement runs
	// regardless of `want`.
	if want == 0 && b.GetSpec().GetStrategy().GetKind() != pb.StrategyKind_DAEMONSET {
		return 0, nil
	}

	status := c.loadStatus(ctx, e.Key)
	// §4.4 node-failure rescheduling runs before everything else: mark
	// Lost past the grace period, retire (free the index) when a
	// replacement is permitted, then schedule the replacement through
	// the normal strategy path (singleton lease gate applies there).
	freed, err := c.reschedulePass(ctx, &b, e, status, nodes)
	if err != nil {
		return 0, err
	}
	if freed || hasRetired(status) {
		return c.scheduleReplacement(ctx, &b, e, status, nodes, cfg)
	}
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
	// Strategy dispatch (T16): daemonset places per node, singleton
	// schedules behind its cluster lease, everything else through the
	// generic scheduler loop.
	switch b.GetSpec().GetStrategy().GetKind() {
	case pb.StrategyKind_DAEMONSET:
		return c.placeDaemonset(ctx, &b, e, status, nodes)
	case pb.StrategyKind_SINGLETON:
		return c.placeSingleton(ctx, &b, e, status, nodes, cfg)
	}
	return c.placeReplicas(ctx, &b, e, status, nodes, cfg)
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
// per-node (placeDaemonset) and does not use a replica count (V6).
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
