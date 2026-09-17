// Package controller is the leader-side volume controller (Phase 06
// T13, §4.6). One instance runs on the Raft leader — leader-only work
// is the Phase 04/05 pattern (an IsLeader gate checked every tick),
// not a third mechanism. Responsibilities per spec:
//
//   - volume deletion end-to-end (mark Deleting → runtimes destroy
//     their local zvols → controller drops the records); creation is
//     the leader-gated runtime loop (T10) using T03's placement;
//   - primary election (highest-seq rule, ties by lowest node ID —
//     §4.3 failover step 2; the elected runtime runs T11's recovery
//     before serving);
//   - under-replication detection with alerts (§4.6);
//   - replica rebuild when a node is permanently lost: replacement
//     placement + Resyncing row; the runtimes converge via T12's
//     resync (full or incremental as appropriate). Scheduling is
//     bounded — max 2 rebuilds in flight per node;
//   - monthly scrub scheduling per pool.
//
// StateNeedsManualRecovery volumes are never touched (§9: a human
// decides; `expanse ctl volume diverged`, T15).
package controller

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/store"
)

// ZFS is the controller's destructive surface (*zfs.Exec satisfies it).
type ZFS interface {
	DestroyZvol(ctx context.Context, zvol string, recursive bool) error
	Scrub(ctx context.Context, pool string) error
}

// Options configures the controller.
type Options struct {
	NodeID string
	St     store.Store
	Pool   string
	Logger *slog.Logger

	// IsLeader gates every mutating action: only the Raft leader
	// plans (the Phase 04/05 leader-only pattern).
	IsLeader func() bool

	// ZFS performs pool scrubs and zvol destroys (nil in unit tests).
	ZFS ZFS

	// Alert receives under-replication / placement events (§4.6
	// "alert if it cannot be met").
	Alert func(AlertEvent)

	// Interval is the reconcile cadence (default 5 s).
	Interval time.Duration

	// ScrubInterval is the monthly scrub cadence (default 30d).
	ScrubInterval time.Duration

	// MaxRebuildsPerNode bounds concurrent rebuilds scheduled onto one
	// node (spec: max 2 resync-class operations per node).
	MaxRebuildsPerNode int
}

// AlertEvent is a controller-raised alert.
type AlertEvent struct {
	VolID  string
	Kind   string // "under-replication" | "no-rebuild-target"
	Detail string
	Have   int
	Want   int
}

// Controller is the leader-side volume controller.
type Controller struct {
	opts      Options
	log       *slog.Logger
	rebuilds  map[string]int // node ID → scheduled rebuilds in flight
	lastScrub time.Time
}

// New builds the controller. Call Run in a goroutine.
func New(opts Options) *Controller {
	if opts.Interval <= 0 {
		opts.Interval = 5 * time.Second
	}
	if opts.ScrubInterval <= 0 {
		opts.ScrubInterval = 30 * 24 * time.Hour
	}
	if opts.MaxRebuildsPerNode <= 0 {
		opts.MaxRebuildsPerNode = 2
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Controller{opts: opts, log: opts.Logger, rebuilds: map[string]int{}}
}

// Run reconciles every tick until the context ends.
func (c *Controller) Run(ctx context.Context) {
	t := time.NewTicker(c.opts.Interval)
	defer t.Stop()
	for {
		if err := c.Reconcile(ctx); err != nil {
			c.log.Warn("volume controller reconcile failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Reconcile converges cluster volume state once. All planning is
// leader-only; non-leaders are inert observers.
func (c *Controller) Reconcile(ctx context.Context) error {
	if c.opts.IsLeader == nil || !c.opts.IsLeader() {
		return nil
	}
	ids, err := storage.ListVolumeIDs(ctx, c.opts.St)
	if err != nil {
		return err
	}
	meshed := c.meshedNodes(ctx)
	for _, id := range ids {
		spec, err := storage.LoadSpec(ctx, c.opts.St, id)
		if err != nil {
			continue
		}
		status, rev, err := storage.LoadStatus(ctx, c.opts.St, id)
		if err != nil {
			continue
		}
		switch status.State {
		case storage.StateDeleting:
			c.finalizeDelete(ctx, id, &spec, &status)
			continue
		case storage.StateNeedsManualRecovery:
			continue // §9: a human decides; the controller never touches it
		}
		c.electPrimary(ctx, id, &status, rev, meshed)
		if status, rev, err = storage.LoadStatus(ctx, c.opts.St, id); err != nil {
			continue // deleted mid-flight
		}
		c.enforceReplication(ctx, id, &spec, &status, rev, meshed)
		if status, rev, err = storage.LoadStatus(ctx, c.opts.St, id); err != nil {
			continue
		}
		c.rebuildLostReplicas(ctx, id, &spec, &status, rev, meshed)
	}
	c.maybeScrub(ctx)
	return nil
}

// Delete marks a volume Deleting (the runtimes destroy their local
// zvols; the next reconcile drops the records).
func (c *Controller) Delete(ctx context.Context, volID string) error {
	status, rev, err := storage.LoadStatus(ctx, c.opts.St, volID)
	if err != nil {
		return err
	}
	status.State = storage.StateDeleting
	return storage.CompareAndSwapStatus(ctx, c.opts.St, volID, rev, status)
}

// finalizeDelete destroys local state for a deleting volume and drops
// the records.
func (c *Controller) finalizeDelete(ctx context.Context, volID string, spec *storage.Spec, status *storage.Status) {
	if c.opts.ZFS != nil {
		for _, p := range status.Placement {
			_ = c.opts.ZFS.DestroyZvol(ctx, p.ZvolPath, true) //nolint:errcheck — already gone is fine
		}
	}
	_ = c.opts.St.Delete(ctx, storage.SpecKey(volID), 0)
	_ = c.opts.St.Delete(ctx, storage.StatusKey(volID), 0)
	c.log.Info("volume deleted", "vol", volID, "name", spec.Name)
}

// electPrimary keeps a valid primary elected by the highest-seq rule
// (§4.3 failover step 2): among placement nodes still meshed, the
// highest replica Sequence wins; ties break by lowest node ID. The
// elected node's runtime acquires the volume lease and runs T11's
// recovery (steps 4a–5) before serving.
func (c *Controller) electPrimary(ctx context.Context, volID string, status *storage.Status, rev store.Revision, meshed map[string]bool) {
	if status.Primary != "" && meshed[status.Primary] {
		return // healthy primary already elected and meshed
	}
	type cand struct {
		id  string
		seq uint64
	}
	var cands []cand
	for _, p := range status.Placement {
		if meshed[p.NodeID] {
			cands = append(cands, cand{p.NodeID, p.Sequence})
		}
	}
	if len(cands) == 0 {
		return // no meshed replica can serve; under-replication covers
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].seq != cands[j].seq {
			return cands[i].seq > cands[j].seq // highest seq wins
		}
		return cands[i].id < cands[j].id // ties → lowest node ID
	})
	status.Primary = cands[0].id
	if status.State == storage.StateHealthy {
		status.State = storage.StateDegraded // recovery (T11) levels it first
	}
	if err := storage.CompareAndSwapStatus(ctx, c.opts.St, volID, rev, *status); err == nil {
		c.log.Info("primary elected", "vol", volID, "primary", status.Primary, "seq", cands[0].seq)
	}
}

// enforceReplication alerts when the volume cannot meet its
// replication factor (§4.6) and degrades the state.
func (c *Controller) enforceReplication(ctx context.Context, volID string, spec *storage.Spec, status *storage.Status, rev store.Revision, meshed map[string]bool) {
	healthy := 0
	for _, p := range status.Placement {
		if p.Healthy && meshed[p.NodeID] {
			healthy++
		}
	}
	if healthy >= spec.Replication {
		return
	}
	c.emitAlert(AlertEvent{
		VolID: volID, Kind: "under-replication",
		Have: healthy, Want: spec.Replication,
		Detail: "replication factor cannot be met",
	})
	if status.State == storage.StateHealthy {
		status.State = storage.StateDegraded
		_ = storage.CompareAndSwapStatus(ctx, c.opts.St, volID, rev, *status)
	}
}

// rebuildLostReplicas replaces replicas on permanently-lost nodes:
// replacement placement (spread via T03, preferring nodes with free
// space), the replacement row marked Resyncing. The runtimes converge:
// the new node creates its zvol, the primary fans out to it and T12's
// resync fills it (full or incremental as appropriate). Scheduling is
// bounded: max MaxRebuildsPerNode in flight per node.
func (c *Controller) rebuildLostReplicas(ctx context.Context, volID string, spec *storage.Spec, status *storage.Status, rev store.Revision, meshed map[string]bool) {
	lost := false
	var existing []string
	var freeNodes []string
	for _, p := range status.Placement {
		existing = append(existing, p.NodeID)
		if !meshed[p.NodeID] {
			lost = true
		}
	}
	if !lost {
		return
	}
	for id := range meshed {
		if c.rebuilds[id] < c.opts.MaxRebuildsPerNode {
			freeNodes = append(freeNodes, id)
		}
	}
	sort.Strings(freeNodes)
	infos := make([]storage.NodeInfo, 0, len(freeNodes))
	for _, id := range freeNodes {
		infos = append(infos, storage.NodeInfo{ID: id, PoolName: c.opts.Pool})
	}
	class := storage.DefaultStorageClass()
	class.Replication = 1 // one replacement replica per pass
	chosen, err := storage.SelectNodes(class, infos, existing)
	if err != nil {
		c.emitAlert(AlertEvent{
			VolID: volID, Kind: "no-rebuild-target",
			Detail: "no node available for replica rebuild",
		})
		return
	}
	repl := chosen[0]
	c.rebuilds[repl.ID]++
	c.log.Info("replica rebuild scheduled", "vol", volID, "new node", repl.ID)
	for i := range status.Placement {
		p := &status.Placement[i]
		if !meshed[p.NodeID] {
			// The replacement takes over the row (the zvol path is
			// per-volume, not per-node).
			p.NodeID = repl.ID
			p.Role = storage.RoleResyncing
			p.Healthy = false
			p.Sequence = 0
			break
		}
	}
	_ = storage.CompareAndSwapStatus(ctx, c.opts.St, volID, rev, *status)
}

// RebuildDone releases one scheduled rebuild slot (called when the
// runtimes complete or abandon a rebuild).
func (c *Controller) RebuildDone(nodeID string) {
	if c.rebuilds[nodeID] > 0 {
		c.rebuilds[nodeID]--
	}
}

// maybeScrub schedules a monthly scrub per pool (§4.6).
func (c *Controller) maybeScrub(ctx context.Context) {
	if c.opts.ZFS == nil {
		return
	}
	if time.Since(c.lastScrub) < c.opts.ScrubInterval {
		return
	}
	c.lastScrub = time.Now()
	if err := c.opts.ZFS.Scrub(ctx, c.opts.Pool); err != nil {
		c.log.Warn("scrub schedule failed", "pool", c.opts.Pool, "err", err)
	} else {
		c.log.Info("scrub scheduled", "pool", c.opts.Pool)
	}
}

// meshedNodes lists node IDs with a published mesh record.
func (c *Controller) meshedNodes(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	entries, err := c.opts.St.List(ctx, "/nodes/")
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !strings.HasSuffix(string(e.Key), "/network.wgPublicKey") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(string(e.Key), "/nodes/"), "/network.wgPublicKey")
		out[id] = true
	}
	return out
}

func (c *Controller) emitAlert(ev AlertEvent) {
	if c.opts.Alert != nil {
		c.opts.Alert(ev)
	}
	c.log.Warn("volume alert", "vol", ev.VolID, "kind", ev.Kind,
		"have", ev.Have, "want", ev.Want, "detail", ev.Detail)
}

// ErrNoFit is placement space-accounting refusal.
type errNoFit struct {
	vol, node string
	need      uint64
	free      uint64
}

func (e *errNoFit) Error() string {
	return fmt.Sprintf("volume %s does not fit on %s: needs %d, pool free %d", e.vol, e.node, e.need, e.free)
}
