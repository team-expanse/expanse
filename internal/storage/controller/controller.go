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
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/expanse/expanse/internal/cluster/lease"
	"github.com/expanse/expanse/internal/quantity"
	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/storage/drbd"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
	pbproto "google.golang.org/protobuf/proto"
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

	// ProbeSeq asks a candidate node for the LIVE last sequence of its
	// durable copy of volID (recovery 4a QuerySeq over the exvol
	// transport). When set, election requires probe evidence of
	// currency — the store's placement.Sequence is written
	// asynchronously and is not trusted. nil (unit tests) falls back
	// to placement.Sequence.
	ProbeSeq func(ctx context.Context, volID, nodeID string) (uint64, error)

	// Alloc hands out each volume's DRBD minor, port and node-ids at creation.
	Alloc *drbd.Allocator

	// NoCandidateRounds is how many consecutive election rounds with
	// zero live probe evidence flag NeedsManualRecovery (§9). Default
	// 3 — one bad probe round must not flip a volume to manual mode.
	NoCandidateRounds int
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

	noCandRounds map[string]int // volID → consecutive no-evidence rounds

	latchNoted map[string]time.Time // volID → when a stuck latch was last reported
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
	return &Controller{opts: opts, log: opts.Logger, rebuilds: map[string]int{}, noCandRounds: map[string]int{}, latchNoted: map[string]time.Time{}}
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
	leader := c.opts.IsLeader != nil && c.opts.IsLeader()
	c.log.Info("controller: reconcile tick", "leader", leader)
	if !leader {
		return nil
	}
	meshed, err := c.meshedNodes(ctx)
	if err != nil {
		return fmt.Errorf("controller: cannot read node membership; skipping this round: %w", err)
	}
	c.processPending(ctx, meshed)
	ids, err := storage.ListVolumeIDs(ctx, c.opts.St)
	if err != nil {
		return err
	}
	if err := c.reconcileBlocks(ctx, meshed); err != nil {
		c.log.Warn("block volume reconcile failed", "err", err)
	}
	if err := c.processVolumeOps(ctx, ids); err != nil {
		c.log.Warn("volume ops failed", "err", err)
	}
	for _, id := range ids {
		spec, err := storage.LoadSpec(ctx, c.opts.St, id)
		if err != nil {
			c.log.Warn("reconcile: load spec failed; skipping volume this round", "vol", id, "err", err)
			continue
		}
		status, rev, err := storage.LoadStatus(ctx, c.opts.St, id)
		if err != nil {
			c.log.Warn("reconcile: load status failed; skipping volume this round", "vol", id, "err", err)
			continue
		}
		switch status.State {
		case storage.StateDeleting:
			c.finalizeDelete(ctx, id, &spec, &status)
			continue
		case storage.StateNeedsManualRecovery:
			// §9: a human decides; the controller never touches a
			// DIVERGED volume. But a latch set by the no-candidate path
			// (transient multi-node churn: leases lapse under raft
			// leader loss, nodes reboot) must not turn into permanent
			// unavailability — when a meshed placement node presents
			// live probe evidence, the volume is provably intact and
			// election may proceed.
			if !storage.AutoLatched(ctx, c.opts.St, id) {
				c.noteLatched(id, "latched by recovery or an operator (not by the no-candidate path)")
				continue
			}
			live, why := c.liveCandidateExists(ctx, id, &status, meshed)
			if !live {
				c.noteLatched(id, why)
				continue
			}
			c.noCandRounds[id] = 0
			status.State = storage.StateDegraded
			if err := storage.CompareAndSwapStatus(ctx, c.opts.St, id, rev, status); err == nil {
				storage.ClearAutoLatch(ctx, c.opts.St, id)
				c.log.Info("manual-recovery latch cleared: live candidate presented probe evidence", "vol", id)
			} else {
				c.log.Warn("manual-recovery latch: clearing CAS failed; will retry", "vol", id, "err", err)
			}
			continue
		}
		c.electPrimary(ctx, id, &status, rev, meshed)
		if status, rev, err = storage.LoadStatus(ctx, c.opts.St, id); err != nil {
			c.log.Warn("reconcile: reload status after electPrimary failed; skipping rest of round", "vol", id, "err", err)
			continue // deleted mid-flight
		}
		_ = rev
		c.enforceReplication(ctx, id, &spec, &status, rev, meshed)
		if status, rev, err = storage.LoadStatus(ctx, c.opts.St, id); err != nil {
			c.log.Warn("reconcile: reload status after enforceReplication failed; skipping rebuild check", "vol", id, "err", err)
			continue
		}
		c.rebuildLostReplicas(ctx, id, &spec, &status, rev, meshed)
	}
	c.maybeScrub(ctx)
	return nil
}

// processVolumeOps consumes operator requests written by
// `expanse ctl volume` (§4.8): delete and move-primary. Each op record
// is one JSON value under /volumes/_ops/<kind>/<volID>.
func (c *Controller) processVolumeOps(ctx context.Context, ids []string) error {
	type op struct {
		Target    string `json:"target"`    // volume NAME (CLI-side lookup)
		To        string `json:"to"`        // move-primary destination node
		SizeBytes uint64 `json:"sizeBytes"` // resize target size
	}
	byName, err := c.volumesByName2(ctx, ids)
	if err != nil {
		return err
	}
	for _, kind := range []string{"delete", "move-primary", "resize"} {
		res, err := c.opts.St.List(ctx, store.Key("/volumes/_ops/"+kind+"/"))
		if err != nil {
			continue
		}
		for _, e := range res {
			volID := strings.TrimPrefix(string(e.Key), "/volumes/_ops/"+kind+"/")
			var o op
			if json.Unmarshal(e.Value, &o) == nil && o.Target != "" {
				if id, ok := c.volIDByName(byName, o.Target); ok {
					volID = id
				}
			}
			status, rev, err := storage.LoadStatus(ctx, c.opts.St, volID)
			if err != nil {
				continue // unknown volume; drop below
			}
			switch kind {
			case "delete":
				if err := c.Delete(ctx, volID); err != nil {
					c.log.Warn("delete op failed", "vol", volID, "err", err)
					continue // retry next tick
				}
			case "move-primary":
				// The destination must already hold a replica; the new
				// primary's runtime runs T11 recovery on bring-up.
				legal := false
				for _, p := range status.Placement {
					if p.NodeID == o.To {
						legal = true
						break
					}
				}
				if !legal {
					c.log.Warn("move-primary refused: target holds no replica", "vol", volID, "to", o.To)
				} else if o.To != status.Primary {
					status.Primary = o.To
					if err := storage.CompareAndSwapStatus(ctx, c.opts.St, volID, rev, status); err != nil {
						continue
					}
					c.log.Info("primary moved", "vol", volID, "to", o.To)
				}
			case "resize":
				// The spec is "immutable-except-size" (storage.Spec's own
				// doc comment) — size is the one field the controller
				// mutates outside creation, grow-only (G6.14): the
				// per-node runtimes converge the zvol/device to it.
				spec, specRev, serr := storage.LoadSpecRev(ctx, c.opts.St, volID)
				if serr != nil {
					break
				}
				if o.SizeBytes <= spec.SizeBytes {
					c.log.Warn("resize op refused: not a grow", "vol", volID, "have", spec.SizeBytes, "want", o.SizeBytes)
					break
				}
				spec.SizeBytes = o.SizeBytes
				if err := storage.CompareAndSwapSpec(ctx, c.opts.St, volID, specRev, spec); err != nil {
					c.log.Warn("resize op CAS failed", "vol", volID, "err", err)
					continue // retry next tick
				}
				c.log.Info("volume resized", "vol", volID, "sizeBytes", o.SizeBytes)
			}
			_ = c.opts.St.Delete(ctx, store.Key(string(e.Key)), 0)
		}
	}
	return nil
}

// volumesByName2 maps volume NAME → ID for the op router.
func (c *Controller) volumesByName2(ctx context.Context, ids []string) (map[string]string, error) {
	out := map[string]string{}
	for _, id := range ids {
		spec, err := storage.LoadSpec(ctx, c.opts.St, id)
		if err != nil {
			continue
		}
		out[spec.Name] = id
	}
	return out, nil
}

func (c *Controller) volIDByName(m map[string]string, name string) (string, bool) {
	id, ok := m[name]
	return id, ok
}

// BlockVolumeName is the cluster volume name for a block's storage
// entry (blocks attach to volumes by name).
func BlockVolumeName(ns, block, storage string) string {
	return fmt.Sprintf("blk-%s-%s-%s", ns, block, storage)
}

// reconcileBlocks drives §4.7 steps 1–2 for blocks with storage:
//  1. every storage entry has a cluster volume (creation flows through
//     the T10 pending-create path → T03 placement);
//  2. the volume's PRIMARY lives on a node hosting the block (the
//     scheduler prefers replica-holding nodes via S3; when the block
//     lands elsewhere, the primary moves there — highest-seq rule).
func (c *Controller) reconcileBlocks(ctx context.Context, meshed map[string]bool) error {
	entries, err := c.opts.St.List(ctx, "/blocks/")
	if err != nil {
		return err
	}
	volByName, err := c.volumesByName(ctx)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if strings.HasSuffix(string(e.Key), "/status") {
			continue
		}
		var blk pb.Block
		if pbproto.Unmarshal(e.Value, &blk) != nil {
			continue
		}
		ns, name := splitBlockKey2(string(e.Key))
		for _, s := range blk.GetSpec().GetStorage() {
			vname := BlockVolumeName(ns, name, s.GetName())
			if vol, ok := volByName[vname]; ok {
				// §4.7 step 2: primary co-located with the block.
				c.movePrimaryForBlock(vol, blockNodes(&blk), meshed)
				continue
			}
			// §4.7 step 1: create (idempotent pending request).
			size, err := quantity.ParseBytes(s.GetSize())
			if err != nil {
				continue // validation (V12) rejects earlier
			}
			vspec := &pb.VolumeSpec{
				Name:        vname,
				SizeBytes:   uint64(size.N),
				Class:       s.GetClass(),
				Replication: s.GetReplication(),
			}
			raw, err := pbproto.Marshal(vspec)
			if err != nil {
				continue
			}
			if _, err := c.opts.St.Put(ctx, storage.PendingCreateKey(vname), raw); err != nil {
				return err
			}
			c.log.Info("block volume create requested", "block", ns+"/"+name, "vol", vname)
		}
	}
	return nil
}

// movePrimaryForBlock elects the volume primary among the nodes that
// host the block (highest replica Sequence, lowest node ID on ties).
func (c *Controller) movePrimaryForBlock(vol storage.Status, block map[string]bool, meshed map[string]bool) {
	if vol.Primary != "" && block[vol.Primary] {
		return // primary already co-located
	}
	type cand struct {
		id  string
		seq uint64
		rev store.Revision
	}
	var best *cand
	for _, p := range vol.Placement {
		if !block[p.NodeID] || !meshed[p.NodeID] {
			continue
		}
		if best == nil || p.Sequence > best.seq || (p.Sequence == best.seq && p.NodeID < best.id) {
			best = &cand{id: p.NodeID, seq: p.Sequence}
		}
	}
	if best == nil {
		return // the block lives nowhere we hold a replica yet
	}
	c.log.Info("primary moves to block host", "vol_primary", vol.Primary, "to", best.id, "seq", best.seq)
}

// blockNodes maps a block's active placement node IDs.
func blockNodes(blk *pb.Block) map[string]bool {
	out := map[string]bool{}
	if status := blk.GetStatus(); status != nil {
		for _, p := range status.GetPlacements() {
			if p.GetReplicaIndex() >= 0 && p.GetPhase() != pb.Phase_LOST {
				out[p.GetNodeId()] = true
			}
		}
	}
	return out
}

// volumesByName loads every volume's status keyed by NAME.
func (c *Controller) volumesByName(ctx context.Context) (map[string]storage.Status, error) {
	out := map[string]storage.Status{}
	ids, err := storage.ListVolumeIDs(ctx, c.opts.St)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		spec, err := storage.LoadSpec(ctx, c.opts.St, id)
		if err != nil {
			continue
		}
		status, _, err := storage.LoadStatus(ctx, c.opts.St, id)
		if err != nil {
			continue
		}
		out[spec.Name] = status
	}
	return out, nil
}

// splitBlockKey2 splits "/blocks/<ns>/<name>" (bridge's helper, local
// copy to avoid an import cycle).
func splitBlockKey2(k string) (ns, name string) {
	rest := strings.TrimPrefix(k, "/blocks/")
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[:i], rest[i+1:]
	}
	return rest, ""
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
// (§4.3 failover step 2). With ProbeSeq wired (production), a
// candidate must present LIVE probe evidence (4a QuerySeq) that its
// durable copy reaches the highest probed sequence — the store's
// placement.Sequence is asynchronous and can elect a node whose zvol
// was lost (the VM runs elected a torn zvol this way). Ties break by
// lowest node ID. The elected node's runtime acquires the volume lease
// and runs T11's recovery (steps 4a–5) before serving.
func (c *Controller) electPrimary(ctx context.Context, volID string, status *storage.Status, rev store.Revision, meshed map[string]bool) {
	if status.Primary != "" {
		primaryMeshed := meshed[status.Primary]
		leaseExpired := primaryMeshed && c.volLeaseExpired(ctx, volID)
		if primaryMeshed && !leaseExpired {
			c.noCandRounds[volID] = 0
			return // healthy primary: meshed, and its lease is either valid
			// or not yet acquired (a fresh election is still mid-handshake)
		}
		c.log.Info("re-electing: current primary is unusable", "vol", volID,
			"primary", status.Primary, "meshed", primaryMeshed, "lease_expired", leaseExpired)
	}
	type cand struct {
		id  string
		seq uint64
	}
	var cands []cand
	if c.opts.ProbeSeq != nil {
		// Currency gate (T17.4): only nodes whose durable copy answers
		// a live probe are eligible, and only at the highest probed
		// sequence — a node whose oplog is behind must be leveled by
		// recovery, not handed the write path. Probes run IN PARALLEL
		// under one round deadline: a sequential probe lets a dead
		// candidate consume the whole budget before a live one is ever
		// asked (observed in the durability VM run — the dead node's
		// dial timeout starved the live replica, refusing elections
		// that had a perfectly good candidate).
		pctx, cancel := context.WithTimeout(ctx, probeBudget)
		type probeRes struct {
			node string
			seq  uint64
			err  error
		}
		var live []string // meshed placement order, for deterministic probe list
		var why []string  // per placement, why it was or was not a candidate (for the refusal log)
		for _, p := range status.Placement {
			switch {
			case !meshed[p.NodeID]:
				why = append(why, p.NodeID+"(not meshed)")
			case p.Role == storage.RoleStale:
				why = append(why, p.NodeID+"(Stale)")
			default:
				live = append(live, p.NodeID)
			}
		}
		results := make([]probeRes, len(live))
		var wg sync.WaitGroup
		for i, node := range live {
			wg.Add(1)
			go func(i int, node string) {
				defer wg.Done()
				seq, err := c.opts.ProbeSeq(pctx, volID, node)
				results[i] = probeRes{node: node, seq: seq, err: err}
			}(i, node)
		}
		wg.Wait()
		cancel()
		for _, pr := range results {
			if pr.err != nil {
				why = append(why, pr.node+"(probe failed)")
				c.log.Warn("election probe failed; candidate not eligible",
					"vol", volID, "node", pr.node, "err", pr.err)
				continue
			}
			cands = append(cands, cand{pr.node, pr.seq})
		}
		if len(cands) == 0 {
			informative := len(live) >= len(status.Placement)/2+1
			c.noCurrentCandidate(ctx, volID, status, rev, strings.Join(why, " "), informative)
			return
		}
		c.noCandRounds[volID] = 0
	} else {
		for _, p := range status.Placement {
			if meshed[p.NodeID] && p.Role != storage.RoleStale {
				cands = append(cands, cand{p.NodeID, p.Sequence})
			}
		}
		if len(cands) == 0 {
			return // no meshed replica can serve; under-replication covers
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].seq != cands[j].seq {
			return cands[i].seq > cands[j].seq // highest seq wins
		}
		return cands[i].id < cands[j].id // ties → lowest node ID
	})
	// Hysteresis: a re-election that changes nothing (same primary,
	// same state) must not CAS — revision churn from a sticky
	// situation re-triggers the runtimes' demotion paths every tick.
	wantState := status.State
	if status.State == storage.StateHealthy && (status.Primary == "" || status.Primary != cands[0].id) {
		wantState = storage.StateDegraded // recovery (T11) levels it first
	}
	if status.Primary == cands[0].id && status.State == wantState {
		return
	}
	status.Primary = cands[0].id
	status.State = wantState
	if err := storage.CompareAndSwapStatus(ctx, c.opts.St, volID, rev, *status); err != nil {
		c.log.Warn("primary election CAS failed; will retry next round", "vol", volID, "primary", status.Primary, "seq", cands[0].seq, "err", err)
	} else {
		c.log.Info("primary elected", "vol", volID, "primary", status.Primary, "seq", cands[0].seq)
	}
}

// probeBudget bounds one full election round's probes; the wired probe
// dial inherits this deadline.
const probeBudget = 3 * time.Second

// noCurrentCandidate is the §9 refusal path: the old primary is gone
// (or fenced) and NO candidate presented live evidence of a current
// durable copy. Electing one anyway would serve a possibly-stale zvol;
// electing nobody keeps the volume read-only-fenced. After
// NoCandidateRounds consecutive evidence-less rounds the volume is
// flagged NeedsManualRecovery — suspected data loss, a human decides.
func (c *Controller) noCurrentCandidate(ctx context.Context, volID string, status *storage.Status, rev store.Revision, why string, informative bool) {
	if informative { // too few probed nodes is a gap in the controller's view, not evidence of loss
		c.noCandRounds[volID]++
	}
	rounds := c.noCandRounds[volID]
	limit := c.opts.NoCandidateRounds
	if limit <= 0 {
		limit = 3
	}
	c.log.Warn("election refused: no candidate with live probe evidence",
		"vol", volID, "rounds", rounds, "primary", status.Primary, "candidates", why)
	if c.opts.Alert != nil {
		c.opts.Alert(AlertEvent{VolID: volID, Kind: "no-current-candidate", Detail: fmt.Sprintf("round %d of %d", rounds, limit)})
	}
	if !informative || rounds < limit {
		return
	}
	if status.State != storage.StateNeedsManualRecovery {
		status.State = storage.StateNeedsManualRecovery
		// Marker first: a latch without it could never lift itself.
		if err := storage.SetAutoLatch(ctx, c.opts.St, volID); err != nil {
			c.log.Warn("election: cannot record the liftable latch marker; not latching", "vol", volID, "err", err)
			return
		}
		if err := storage.CompareAndSwapStatus(ctx, c.opts.St, volID, rev, *status); err == nil {
			c.log.Error("election: no current candidate — manual recovery required", "vol", volID, "rounds", rounds)
		} else {
			storage.ClearAutoLatch(ctx, c.opts.St, volID)
		}
	}
}

// liveCandidateExists reports whether any meshed placement node answers
// a live currency probe. Used ONLY to exit the no-candidate manual-
// recovery latch: presence of a live, provable durable copy is the
// strongest evidence the volume is intact, so keeping the volume
// fenced would trade a transient outage for a permanent one.
func (c *Controller) liveCandidateExists(ctx context.Context, volID string, status *storage.Status, meshed map[string]bool) (bool, string) {
	if c.opts.ProbeSeq == nil {
		return false, "no probe wired"
	}
	pctx, cancel := context.WithTimeout(ctx, probeBudget)
	defer cancel()
	var why []string
	for _, p := range status.Placement {
		if !meshed[p.NodeID] {
			why = append(why, p.NodeID+"(not meshed)")
			continue
		}
		_, err := c.opts.ProbeSeq(pctx, volID, p.NodeID)
		if err == nil {
			return true, ""
		}
		why = append(why, fmt.Sprintf("%s(probe failed: %v)", p.NodeID, err))
	}
	return false, strings.Join(why, " ")
}

// noteLatched says why a latched volume is not being served, at most once a
// minute per volume, so a stuck latch is never silent.
func (c *Controller) noteLatched(volID, why string) {
	if time.Since(c.latchNoted[volID]) < time.Minute {
		return
	}
	c.latchNoted[volID] = time.Now()
	c.log.Warn("volume stays latched for manual recovery", "vol", volID, "why", why)
}

// volLeaseExpired reports whether the volume's primary lease exists
// and has lapsed (15s skew allowance, matching meshedNodes). That is
// the proof the primary died: a rebooted node is meshed again, but its
// volume lease died with the crash (TTL ≪ boot time) — it must
// REPROVE itself via re-election and recovery, not silently resume
// serving a possibly-behind zvol. A MISSING record is NOT expiry: a
// freshly elected primary may not have acquired its lease yet.
func (c *Controller) volLeaseExpired(ctx context.Context, volID string) bool {
	lm := lease.NewManager(c.opts.St, "storage-controller")
	l, ok, err := lm.Inspect(ctx, "exvol-vol-"+volID)
	if err != nil || !ok {
		return false
	}
	return time.Now().After(l.ExpiresAt.Add(15 * time.Second))
}

// enforceReplication derives the volume's live State from its actual
// reachable, healthy replica count (§4.6, G6.11/G6.12): Degraded once
// replicas drop below the replication factor but a write quorum is
// still reachable (still readable+writable); ReadOnly once even a
// write quorum is unreachable — reads still succeed from the primary's
// local copy, writes already fail fast with EIO via the primary's own
// quorum-ack rejection (never hang), this just makes that visible.
// Recovers back to Degraded/Healthy as replicas rejoin and resync —
// unlike a one-way degrade, staying Degraded forever after a rejoin
// would misreport a fully-recovered volume as still impaired.
func (c *Controller) enforceReplication(ctx context.Context, volID string, spec *storage.Spec, status *storage.Status, rev store.Revision, meshed map[string]bool) {
	healthy := 0
	for _, p := range status.Placement {
		if p.Healthy && meshed[p.NodeID] {
			healthy++
		}
	}
	// floor(R/2)+1 — matches the primary's own write-ack quorum
	// (protocol.Primary.Quorum) exactly.
	quorum := spec.Replication/2 + 1
	want := storage.StateHealthy
	switch {
	case healthy < quorum:
		want = storage.StateReadOnly
	case healthy < spec.Replication:
		want = storage.StateDegraded
	}
	if want != storage.StateHealthy {
		c.emitAlert(AlertEvent{
			VolID: volID, Kind: "under-replication",
			Have: healthy, Want: spec.Replication,
			Detail: "replication factor cannot be met",
		})
	}
	switch status.State {
	case storage.StateHealthy, storage.StateDegraded, storage.StateReadOnly:
		if status.State != want {
			status.State = want
			_ = storage.CompareAndSwapStatus(ctx, c.opts.St, volID, rev, *status)
		}
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

// meshedNodes lists node IDs considered alive for placement and
// election decisions. A node publishes a mesh record that PERSISTS
// through a hard kill (qemu quit never unpublishes anything), so the
// record alone cannot mean "alive". Each node therefore also holds a
// renewable liveness lease (/leases/node-<id>, §4.3 machinery); a
// record whose lease has EXPIRED is a dead node. Nodes that publish no
// lease at all (single-bolt clusters, tests) fall back to the record —
// the pre-lease semantics.
func (c *Controller) meshedNodes(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	entries, err := c.opts.St.List(ctx, "/nodes/")
	if err != nil {
		return nil, err
	}
	lm := lease.NewManager(c.opts.St, "storage-controller")
	now := time.Now()
	for _, e := range entries {
		if !strings.HasSuffix(string(e.Key), "/network.wgPublicKey") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(string(e.Key), "/nodes/"), "/network.wgPublicKey")
		alive := true
		if l, ok, ierr := lm.Inspect(ctx, "node-"+id); ierr == nil && ok {
			// 15s skew allowance: expiry is judged against the local
			// clock, the grant was made on the holder's.
			if now.After(l.ExpiresAt.Add(15 * time.Second)) {
				alive = false
			}
		}
		out[id] = alive
	}
	return out, nil
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
