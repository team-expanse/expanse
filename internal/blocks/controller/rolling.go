// Rolling update engine (PHASE04.md §5.2, T15).
//
// The algorithm runs as a step function inside the controller's
// reconcile pass, one §5.2 action per pass, persisting BlockStatus after
// each action so a crash resumes cleanly. All timing (drain window,
// minReadySeconds, readiness polling) goes through UpdateHooks, which
// tests replace with a fake clock and a scripted prober — the T12
// lesson: timing-based tests are racy by construction, so the state
// machine is driven synchronously through seams instead.
//
// State encoding (proto-binary pb.BlockStatus at /blocks/<ns>/<name>/status):
//   - Block phase UPDATING while a roll is in flight; back to RUNNING on
//     success, DEGRADED on failure (plus an UpdateAborted condition when
//     auto-rollback is disabled or has run — the roll is not retried
//     until the spec changes again, i.e. the block's store revision moves).
//   - Per-replica generation in PlacementStatus.Generation = the block's
//     store revision it was last started at. A roll is needed when any
//     placement's generation differs from the current block revision.
//   - A replica being drained holds Phase DRAINING for the duration of
//     the drain window (LB pool removal itself is Phase 05; this card
//     implements the flag and the wait).
package controller

import (
	"context"
	"sort"
	"strconv"
	"time"

	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/scheduler"
	pb "github.com/expanse/expanse/proto"
)

// §5.2 defaults.
const (
	DefaultConnectionDrain = 15 * time.Second
	DefaultMaxUnavailable  = 1
	// ReadyTimeout bounds the per-replica "wait until ready" loop.
	ReadyTimeout = 5 * time.Minute
	// pollStep is the readiness poll interval inside waitReady.
	pollStep = time.Second
)

// UpdateHooks are the rolling-update seams. A nil hook field gets a
// permissive default (Ready=true, Start/Stop no-op), so the controller
// runs its state machine end-to-end even before the Phase 05 runtime
// adapters exist.
type UpdateHooks struct {
	// Ready reports whether a replica is currently serving. The
	// minReadySeconds wait requires Ready to hold continuously across
	// the window (via the Now/Sleep pair).
	Ready func(ctx context.Context, b *pb.Block, p *pb.PlacementStatus) bool
	// Start provisions a replica at p.Generation (systemd unit start in
	// production; a recording seam in tests).
	Start func(ctx context.Context, b *pb.Block, p *pb.PlacementStatus) error
	// Stop tears a replica down.
	Stop func(ctx context.Context, b *pb.Block, p *pb.PlacementStatus) error
	// Now returns the current time (fake clock in tests).
	Now func() time.Time
	// Sleep advances time: real sleep in production, fake-clock step in
	// tests. Drains and readiness waits both go through here.
	Sleep func(d time.Duration)
}

func (h *UpdateHooks) ready(ctx context.Context, b *pb.Block, p *pb.PlacementStatus) bool {
	if h.Ready == nil {
		return true
	}
	return h.Ready(ctx, b, p)
}

func (h *UpdateHooks) start(ctx context.Context, b *pb.Block, p *pb.PlacementStatus) error {
	if h.Start == nil {
		return nil
	}
	return h.Start(ctx, b, p)
}

func (h *UpdateHooks) stop(ctx context.Context, b *pb.Block, p *pb.PlacementStatus) error {
	if h.Stop == nil {
		return nil
	}
	return h.Stop(ctx, b, p)
}

func (h *UpdateHooks) now() time.Time {
	if h.Now == nil {
		return time.Now()
	}
	return h.Now()
}

func (h *UpdateHooks) sleep(d time.Duration) {
	if d <= 0 {
		return
	}
	if h.Sleep == nil {
		time.Sleep(d)
		return
	}
	h.Sleep(d)
}

// abortCondition marks a rolled-back/aborted update; the recorded block
// revision in Reason prevents retry loops until the spec changes again.
const abortCondition = "UpdateAborted"

// updateNeeded reports whether any placement is not at the block's
// current revision (its §5.2 target generation).
func updateNeeded(status *pb.BlockStatus, target int64) bool {
	for _, p := range status.GetPlacements() {
		if p.GetGeneration() != target {
			return true
		}
	}
	return false
}

// updatePass performs at most one §5.2 action for a block and reports
// whether the status record changed. target is the block's store
// revision; want the desired replica count.
func (c *Controller) updatePass(ctx context.Context, b *pb.Block, status *pb.BlockStatus,
	target int64, want int, nodes []scheduler.NodeView, cfg scheduler.OvercommitConfig,
) (bool, error) {
	h := c.hooks()
	upd := b.GetSpec().GetStrategy().GetUpdate()

	// Aborted roll: stay Degraded until the spec changes (new revision).
	if cond := findCondition(status, abortCondition); cond != nil &&
		cond.GetReason() == strconv.FormatInt(target, 10) {
		return false, nil
	}

	if status.GetPhase() != pb.Phase_UPDATING {
		status.Phase = pb.Phase_UPDATING
		setCondition(status, "Updating", true, "rolling update to generation "+strconv.FormatInt(target, 10))
		return true, nil
	}

	active := activePlacements(status)
	old := oldGenPlacements(status, target)
	updated := 0
	for _, p := range active {
		if p.GetGeneration() == target {
			updated++
		}
	}

	// Surplus cleanup: roll finished (no old-gen placements left) but
	// surge replicas remain — drop the highest-index one. Only runs when
	// the roll itself is done, else it would fight surgeCreate.
	if len(old) == 0 && len(active) > want {
		sort.Slice(active, func(i, j int) bool { return active[i].GetReplicaIndex() > active[j].GetReplicaIndex() })
		for _, p := range active {
			if p.GetGeneration() == target {
				if err := h.stop(ctx, b, p); err != nil {
					return false, errors.Wrap(err, errors.KindInternal, "controller.update", "stop surge replica")
				}
				removePlacement(status, p.GetReplicaIndex())
				return true, nil
			}
		}
	}

	// Roll complete: every placement at target, count reconciled.
	if updated == len(active) && updated == want {
		status.Phase = pb.Phase_RUNNING
		setCondition(status, "Updating", false, "roll to generation "+strconv.FormatInt(target, 10)+" complete")
		return true, nil
	}

	maxUnavail := int(upd.GetMaxUnavailable())
	if maxUnavail <= 0 {
		maxUnavail = DefaultMaxUnavailable
	}
	maxSurge := int(upd.GetMaxSurge())

	// Availability gate: never remove or restart while too much is down.
	readyCount := 0
	for _, p := range active {
		if h.ready(ctx, b, p) {
			readyCount++
		}
	}
	unavailable := len(active) - readyCount
	if unavailable >= maxUnavail {
		return false, nil // §5.2: wait for a replica to become ready
	}

	// Surge path: create a new-generation replica before removing an old
	// one, while under the want+maxSurge capacity.
	if maxSurge > 0 && len(active) < want+maxSurge && len(old) > 0 {
		return c.surgeCreate(ctx, b, status, target, nodes, cfg)
	}
	if len(old) == 0 {
		return false, nil // only surge cleanup pending, handled above
	}

	// In-place path (or surge at capacity): roll the oldest-generation
	// ready replica. Ready replicas are preferred so availability never
	// dips below N - maxUnavailable (the test asserts this invariant).
	r := pickRollReplica(h, ctx, b, old)
	prevGen := r.GetGeneration()

	// Drain: flag + wait (LB pool removal is Phase 05).
	r.Phase = pb.Phase_DRAINING
	h.sleep(drainWindow(upd.GetConnectionDrainSeconds()))
	if err := h.stop(ctx, b, r); err != nil {
		return failUpdate(ctx, h, b, status, r, prevGen, target, upd.GetAutoRollback(),
			errors.Wrap(err, errors.KindInternal, "controller.update", "stop replica"))
	}
	r.Phase = pb.Phase_STARTING
	r.Generation = target
	if err := h.start(ctx, b, r); err != nil {
		return failUpdate(ctx, h, b, status, r, prevGen, target, upd.GetAutoRollback(),
			errors.Wrap(err, errors.KindInternal, "controller.update", "start replica at new generation"))
	}
	// Wait until ready for minReadySeconds (timeout 5 min).
	if !waitReady(h, ctx, b, r, minReady(upd.GetMinReadySeconds())) {
		return failUpdate(ctx, h, b, status, r, prevGen, target, upd.GetAutoRollback(), nil)
	}
	r.Phase = pb.Phase_RUNNING
	return true, nil
}

// surgeCreate adds one new-generation replica at the next free index.
func (c *Controller) surgeCreate(ctx context.Context, b *pb.Block, status *pb.BlockStatus,
	target int64, nodes []scheduler.NodeView, cfg scheduler.OvercommitConfig,
) (bool, error) {
	status.Phase = pb.Phase_UPDATING
	idx := int32(0)
	for _, p := range status.GetPlacements() {
		if p.GetReplicaIndex() >= idx {
			idx = p.GetReplicaIndex() + 1
		}
	}
	var existing []string
	for _, p := range status.GetPlacements() {
		existing = append(existing, p.GetNodeId())
	}
	nodeID, pending := scheduler.Schedule(nodes, scheduler.ReplicaRequest{
		Block: b, ReplicaIndex: int(idx), ExistingPlacements: existing,
	}, cfg, scheduler.ClusterView{SameBlockReplicas: sameBlockReplicas(existing)})
	if pending != nil {
		return false, nil // no room to surge right now; retry next pass
	}
	p := &pb.PlacementStatus{
		ReplicaIndex: idx,
		NodeId:       nodeID,
		Phase:        pb.Phase_STARTING,
		Generation:   target,
	}
	h := c.hooks()
	if err := h.start(ctx, b, p); err != nil {
		return false, errors.Wrap(err, errors.KindInternal, "controller.update", "start surge replica")
	}
	status.Placements = append(status.Placements, p)
	return true, nil
}

// failUpdate marks the block Degraded, rolls back to the previous
// generation when configured, and aborts the roll (guard condition
// records the aborted target revision).
func failUpdate(ctx context.Context, h *UpdateHooks, b *pb.Block, status *pb.BlockStatus,
	r *pb.PlacementStatus, prevGen, target int64, autoRollback bool, cause error,
) (bool, error) {
	r.Phase = pb.Phase_DEGRADED
	status.Phase = pb.Phase_DEGRADED
	msg := "rolling update failed at replica " + strconv.Itoa(int(r.GetReplicaIndex()))
	if cause != nil {
		msg += ": " + cause.Error()
	}
	if autoRollback {
		// Revert this replica to its pre-roll generation.
		r.Phase = pb.Phase_STARTING
		r.Generation = prevGen
		if err := h.start(ctx, b, r); err != nil {
			msg += "; rollback start failed: " + err.Error()
		}
		r.Phase = pb.Phase_RUNNING
		msg += "; rolled back to generation " + strconv.FormatInt(prevGen, 10)
	}
	c := &pb.Condition{
		Type: abortCondition, Status: true, Message: msg,
		Reason: strconv.FormatInt(target, 10),
	}
	if prev := findCondition(status, abortCondition); prev != nil {
		*prev = *c
	} else {
		status.Conditions = append(status.Conditions, c)
	}
	return true, nil
}

// waitReady polls the Ready hook until it has held for minReady
// (continuously — the clock only advances via h.sleep) or times out.
func waitReady(h *UpdateHooks, ctx context.Context, b *pb.Block, p *pb.PlacementStatus, minReady time.Duration) bool {
	start := h.now()
	for {
		if h.ready(ctx, b, p) {
			if h.now().Sub(start) >= minReady {
				return true
			}
		} else {
			// Readiness must hold continuously; reset the window.
			start = h.now()
		}
		if h.now().Sub(start) > ReadyTimeout {
			return false
		}
		h.sleep(pollStep)
	}
}

func minReady(sec int32) time.Duration {
	if sec <= 0 {
		return 0
	}
	return time.Duration(sec) * time.Second
}

func drainWindow(sec int32) time.Duration {
	if sec <= 0 {
		return DefaultConnectionDrain
	}
	return time.Duration(sec) * time.Second
}

// pickRollReplica chooses the oldest-generation replica, preferring
// ready ones (§5.2: only a ready replica's removal can violate
// availability, and the gate above already ensured headroom).
func pickRollReplica(h *UpdateHooks, ctx context.Context, b *pb.Block, old []*pb.PlacementStatus) *pb.PlacementStatus {
	sorted := append([]*pb.PlacementStatus(nil), old...)
	sort.SliceStable(sorted, func(i, j int) bool {
		ri, rj := h.ready(ctx, b, sorted[i]), h.ready(ctx, b, sorted[j])
		if ri != rj {
			return ri && !rj
		}
		if sorted[i].GetGeneration() != sorted[j].GetGeneration() {
			return sorted[i].GetGeneration() < sorted[j].GetGeneration()
		}
		return sorted[i].GetReplicaIndex() < sorted[j].GetReplicaIndex()
	})
	return sorted[0]
}

// oldGenPlacements returns placements not yet at target, excluding
// already-draining ones.
func oldGenPlacements(status *pb.BlockStatus, target int64) []*pb.PlacementStatus {
	var out []*pb.PlacementStatus
	for _, p := range status.GetPlacements() {
		if p.GetGeneration() != target && p.GetPhase() != pb.Phase_DRAINING {
			out = append(out, p)
		}
	}
	return out
}

// activePlacements returns every live placement (no TERMINATING filter
// yet — placements are only removed by this engine).
func activePlacements(status *pb.BlockStatus) []*pb.PlacementStatus {
	return status.GetPlacements()
}

func removePlacement(status *pb.BlockStatus, idx int32) {
	out := status.Placements[:0:0]
	for _, p := range status.Placements {
		if p.GetReplicaIndex() != idx {
			out = append(out, p)
		}
	}
	status.Placements = out
}

func findCondition(st *pb.BlockStatus, typ string) *pb.Condition {
	for _, c := range st.GetConditions() {
		if c.GetType() == typ {
			return c
		}
	}
	return nil
}

func setCondition(st *pb.BlockStatus, typ string, val bool, msg string) {
	if c := findCondition(st, typ); c != nil {
		c.Status = val
		c.Message = msg
		return
	}
	st.Conditions = append(st.Conditions, &pb.Condition{Type: typ, Status: val, Message: msg})
}

// updateNeededLegacy is updateNeeded with the pre-T15 carve-out: a
// placement with Generation 0 predates generation tracking and counts
// as at-target (no surprise roll of blocks written before this card).
func updateNeededLegacy(status *pb.BlockStatus, target int64) bool {
	for _, p := range status.GetPlacements() {
		g := p.GetGeneration()
		if g != 0 && g != target {
			return true
		}
	}
	return false
}
