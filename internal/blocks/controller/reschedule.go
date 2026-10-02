package controller

// Node-failure rescheduling (Phase 04 T17, spec §4.4): when a node
// becomes unreachable, wait unreachable_grace (default 30 s) so
// transient blips do not churn placements; then mark that node's
// placements Lost; then replace them — immediately for stateless
// blocks, behind the StorageAvailable hook for stateful blocks (the
// "never schedule a replacement while the original node might still be
// running it" rule, §4.4), and behind the singleton lease re-acquisition
// for singleton blocks (the old holder's lease will have expired).

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/expanse/expanse/internal/blocks/health"
	"github.com/expanse/expanse/internal/scheduler"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
)

// DefaultUnreachableGrace is the §4.4 grace period before a placement
// on an unreachable node is marked Lost.
const DefaultUnreachableGrace = 30 * time.Second

// storageAvailable reports whether a stateful block's volumes are
// available for a replacement replica. Phase06 TODO wire-up: this is
// the explicit seam for the storage layer's volume-availability
// confirmation; the default returns true (optimistic), matching the
// spec's Phase 06 deferral — NOT a silent gap.
func (c *Controller) storageAvailable(volumeID string) bool {
	if c.StorageAvailable != nil {
		return c.StorageAvailable(volumeID)
	}
	return true
}

// nodeReady reports whether nodeID is present and Ready in the current
// cluster view; absent nodes count as unreachable.
func nodeReady(nodes []scheduler.NodeView, nodeID string) bool {
	for _, n := range nodes {
		if n.ID == nodeID {
			return n.Ready
		}
	}
	return false
}

// reschedulePass applies §4.4 to one block's placements. It marks Lost
// placements (after the grace period) and retires them (frees the
// replica index) only when a replacement is actually permitted:
// immediately for stateless blocks, behind StorageAvailable for stateful
// ones. The caller then schedules replacements through the normal
// strategy dispatch — which is where the singleton lease gate applies
// (the old holder's lease will have expired by then per the Phase 03
// safety argument).
//
// The first-unreachable observation is in-memory (leader-local): a
// leader failover resets the grace clock, which is safe — the grace
// exists to ride out blips, and a fresh leader re-observes before
// acting. Returns whether the status record changed.
func (c *Controller) reschedulePass(ctx context.Context, b *pb.Block, e store.Entry, status *pb.BlockStatus, nodes []scheduler.NodeView) (bool, error) {
	grace := c.UnreachableGrace
	if grace <= 0 {
		grace = DefaultUnreachableGrace
	}
	now := c.hooks().now()

	stateful := len(b.GetSpec().GetStorage()) > 0
	changed, failed := false, false
	for _, p := range status.GetPlacements() {
		if p.GetReplicaIndex() == -1 {
			continue // already retired: replacement handled elsewhere
		}
		if nodeReady(nodes, p.GetNodeId()) {
			c.unreachableMu.Lock()
			delete(c.unreachableSince, string(e.Key)+"\x00"+p.GetNodeId())
			c.unreachableMu.Unlock()
			moved, marked := c.livenessPass(ctx, b, e.Key, p, stateful)
			changed = changed || moved || marked
			failed = failed || marked
			continue
		}
		ckey := string(e.Key) + "\x00" + p.GetNodeId()
		if p.GetPhase() != pb.Phase_LOST {
			// §4.4 step 1–2: wait the grace window, then mark Lost.
			c.unreachableMu.Lock()
			since, seen := c.unreachableSince[ckey]
			if !seen {
				c.unreachableSince[ckey] = now
			}
			c.unreachableMu.Unlock()
			if !seen || now.Sub(since) < grace {
				continue // inside the grace window: do nothing
			}
			p.Phase = pb.Phase_LOST
			changed = true
		}
		// §4.4 steps 3–4: retire (free the index) only when a replacement
		// is permitted now. Gate-blocked Lost records re-enter here on
		// every pass, so a later storage-available signal unblocks them.
		if stateful && !c.storageAvailable(p.GetNodeId()) {
			// Volume not confirmed available elsewhere: keep the Lost
			// record, keep the index — no replacement while the original
			// node might still be running the replica. (Phase 06 wires
			// the real volume check; see storageAvailable.)
			continue
		}
		p.ReplicaIndex = -1 // retire: index freed for the replacement
		changed = true
		c.unreachableMu.Lock()
		delete(c.unreachableSince, ckey)
		c.unreachableMu.Unlock()
	}
	if !changed {
		return false, nil
	}
	status.Phase = pb.Phase_DEGRADED
	status.PendingReason = &pb.PendingReason{
		Code:    "Rescheduling",
		Message: "replacing placements on unreachable nodes",
	}
	if failed {
		status.PendingReason.Message = "replacing replicas that failed their liveness probe"
	}
	if err := c.persistStatus(ctx, e.Key, status); err != nil {
		return false, err
	}
	return true, nil
}

// livenessPass marks p FAILED once its own node gives up restarting it, then retires it so
// the replacement lands elsewhere (behind the storage gate). A daemonset replica stays put.
func (c *Controller) livenessPass(ctx context.Context, b *pb.Block, k store.Key, p *pb.PlacementStatus, stateful bool) (moved, marked bool) {
	if p.GetPhase() != pb.Phase_FAILED {
		rec, err := health.LivenessFailedOn(ctx, c.St, k, p.GetReplicaIndex(), p.GetNodeId())
		if err != nil && c.Logger != nil {
			c.Logger.Warn("read replica liveness", "block", string(k), "replica", p.GetReplicaIndex(), "err", err)
		}
		if rec == nil {
			return false, false // unknown is not unhealthy
		}
		p.Phase, marked = pb.Phase_FAILED, true
	}
	if b.GetSpec().GetStrategy().GetKind() == pb.StrategyKind_DAEMONSET {
		return false, marked
	}
	if stateful && !c.storageAvailable(p.GetNodeId()) {
		return false, marked
	}
	p.FormerIndex, p.ReplicaIndex = p.GetReplicaIndex(), -1
	return true, marked
}

// livenessNodesBeforeStop is how many nodes a replica may fail its liveness probe on before
// it is stopped rather than moved again, so a broken workload does not cycle through the cluster.
const livenessNodesBeforeStop = 2

// stoppedReplicas lists the replica indexes of this revision that failed on too many nodes.
func stoppedReplicas(status *pb.BlockStatus, revision int64) map[int32]bool {
	fails := map[int32]int{}
	for _, p := range status.GetPlacements() {
		if p.GetReplicaIndex() == -1 && p.GetPhase() == pb.Phase_FAILED && p.GetGeneration() == revision {
			fails[p.GetFormerIndex()]++
		}
	}
	out := map[int32]bool{}
	for i, n := range fails {
		if n >= livenessNodesBeforeStop {
			out[i] = true
		}
	}
	return out
}

// markStopped sets the block FAILED, or DEGRADED while other replicas still run, and says why.
func markStopped(status *pb.BlockStatus, stopped map[int32]bool) {
	status.Phase = pb.Phase_FAILED
	for _, p := range status.GetPlacements() {
		if p.GetReplicaIndex() >= 0 {
			status.Phase = pb.Phase_DEGRADED
		}
	}
	idx := slices.Sorted(maps.Keys(stopped))
	names := make([]string, len(idx))
	for i, n := range idx {
		names[i] = strconv.Itoa(int(n))
	}
	status.PendingReason = &pb.PendingReason{
		Code: "LivenessFailed",
		Message: fmt.Sprintf("replica %s failed its liveness probe on %d nodes and was stopped",
			strings.Join(names, ", "), livenessNodesBeforeStop),
	}
}

// livenessFailedNodes lists the nodes whose replica of this revision failed its liveness probe.
func livenessFailedNodes(status *pb.BlockStatus, revision int64) map[string]bool {
	out := map[string]bool{}
	for _, p := range status.GetPlacements() {
		if p.GetReplicaIndex() == -1 && p.GetPhase() == pb.Phase_FAILED && p.GetGeneration() == revision {
			out[p.GetNodeId()] = true
		}
	}
	return out
}

// withoutNodes returns nodes minus the excluded IDs; nodes itself when none are excluded.
func withoutNodes(nodes []scheduler.NodeView, excluded map[string]bool) []scheduler.NodeView {
	if len(excluded) == 0 {
		return nodes
	}
	out := make([]scheduler.NodeView, 0, len(nodes))
	for _, n := range nodes {
		if !excluded[n.ID] {
			out = append(out, n)
		}
	}
	return out
}

// scheduleReplacement places replacements for retired (index -1) Lost
// placements through the normal strategy path. Singleton blocks pass
// the lease gate here: if the lease is still live elsewhere the
// replacement waits (never double-places); once the old holder's lease
// has expired the acquire takes over and exactly one placement exists.
func (c *Controller) scheduleReplacement(ctx context.Context, b *pb.Block, e store.Entry, status *pb.BlockStatus, nodes []scheduler.NodeView, cfg scheduler.OvercommitConfig) (int, error) {
	switch b.GetSpec().GetStrategy().GetKind() {
	case pb.StrategyKind_DAEMONSET:
		return c.placeDaemonset(ctx, b, e, status, nodes)
	case pb.StrategyKind_SINGLETON:
		return c.placeSingleton(ctx, b, e, status, nodes, cfg)
	}
	return c.placeReplicas(ctx, b, e, status, nodes, cfg)
}

// hasRetired reports whether the status carries a retired Lost placement
// awaiting replacement.
func hasRetired(status *pb.BlockStatus) bool {
	for _, p := range status.GetPlacements() {
		if p.GetReplicaIndex() == -1 && p.GetPhase() == pb.Phase_LOST {
			return true
		}
	}
	return false
}
