package controller

// Runtime promotion (T20.5b): the bridge between persisted placements
// and observed reality. The leader-side desired-state bridge (wire
// package) materializes each SCHEDULING placement as a block-replica
// resource under /node/<node>/resources/; the target node's agent
// reconciles it into a systemd unit and publishes per-resource status
// under /node/<node>/status/resources/<id> ("health=healthy in_sync=true").
// RuntimePass reads those records and promotes placements to RUNNING,
// then the block itself when every replica is there (§4.3 converge).
//
// Deliberately minimal: no STARTING dwell in the steady-state path —
// in_sync+healthy IS running (the unit reached active). STARTING remains
// a rolling-update-only phase (§5.2), whose Ready hook decides promotion
// mid-update. Health-driven demotion (RUNNING→DEGRADED) is likewise out:
// the reschedule pass owns failure handling (§4.4), fed by the same
// records going unhealthy.

import (
	"context"
	"strconv"
	"strings"

	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
)

// ReplicaResourceID is the desired-state resource id the bridge writes
// for one placement (also the agent status key suffix).
func ReplicaResourceID(namespace, name string, index int) string {
	return "block-replica:" + namespace + "/" + name + "/" + strconv.Itoa(index)
}

// RuntimePass promotes placements whose replica units report healthy and
// in sync on their node, and the block phase when all replicas are
// RUNNING. Idempotent; safe to run before placement in every pass.
func (c *Controller) RuntimePass(ctx context.Context) error {
	entries, err := c.St.List(ctx, "/blocks/")
	if err != nil {
		return errors.Wrap(err, errors.KindInternal, "controller.RuntimePass", "list blocks")
	}
	for _, e := range entries {
		if isStatusKey(e.Key) {
			continue
		}
		if err := c.promoteBlock(ctx, *e); err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) promoteBlock(ctx context.Context, e store.Entry) error {
	k := e.Key
	sk := statusKey(k)
	se, err := c.St.Get(ctx, sk)
	if err != nil {
		return nil // no status yet: nothing placed
	}
	var status pb.BlockStatus
	if err := proto.Unmarshal(se.Value, &status); err != nil {
		return errors.Wrap(err, errors.KindInternal, "controller.RuntimePass", "unmarshal "+string(sk))
	}
	// Desired replica count decides whether the block itself is done.
	var blk pb.Block
	if err := proto.Unmarshal(e.Value, &blk); err != nil {
		return errors.Wrap(err, errors.KindInternal, "controller.RuntimePass", "unmarshal "+string(k))
	}
	want := replicaCount(&blk)
	changed := false
	active, running := 0, 0
	for _, p := range status.GetPlacements() {
		if p.GetReplicaIndex() < 0 || p.GetNodeId() == "" {
			continue // retired records never block promotion
		}
		active++
		if p.GetPhase() == pb.Phase_RUNNING {
			running++
			continue
		}
		// SCHEDULING placements await the agent's first health report;
		// STARTING ones (created by the §5.2 surge/scale-up path) are
		// promoted the same way — the agent reports health either way.
		if p.GetPhase() != pb.Phase_SCHEDULING && p.GetPhase() != pb.Phase_STARTING {
			continue
		}
		ok, err := c.replicaUp(ctx, p.GetNodeId(), statusKey2ns(k), statusKey2name(k), int(p.GetReplicaIndex()))
		if err != nil {
			return err
		}
		if ok {
			p.Phase = pb.Phase_RUNNING
			running++
			changed = true
		}
	}
	// Block phase flips forward only when exactly the desired set is
	// live and every one of them is RUNNING — during a surge there are
	// want+1 live placements and the roll owns the phase (§5.2); a
	// premature RUNNING would stop placeBlock from dispatching the
	// remaining update steps. Never back: DEGRADED belongs to the
	// reschedule pass (§4.4). Daemonsets (want == 0, V6) promote when
	// every live placement is RUNNING (active == running > 0) — the
	// per-node set changes without a phase reset (§4.4: daemonsets
	// place regardless of drain state, extension is additive).
	done := want > 0 && active == want && running == want
	if want == 0 {
		done = active > 0 && running == active
	}
	if done &&
		status.GetPhase() != pb.Phase_RUNNING &&
		// Mid-roll placements can all be RUNNING but at the OLD
		// generation — the §5.2 roll owns the phase until every
		// replica is at the target revision, else the flip-flop
		// (RuntimePass RUNNING / updatePass UPDATING) starves the roll.
		func() bool {
			for _, p := range status.GetPlacements() {
				if p.GetReplicaIndex() < 0 || p.GetNodeId() == "" {
					continue
				}
				if p.GetGeneration() != int64(e.Revision) {
					return false
				}
			}
			return true
		}() {
		status.Phase = pb.Phase_RUNNING
		changed = true
	}
	if !changed {
		return nil
	}
	out, err := proto.Marshal(&status)
	if err != nil {
		return errors.Wrap(err, errors.KindInternal, "controller.RuntimePass", "marshal status")
	}
	if _, err := c.St.Txn(ctx, []store.Op{
		{Kind: store.OpPut, Key: sk, Value: out},
	}); err != nil {
		return errors.Wrap(err, errors.KindUnavailable, "controller.RuntimePass", "persist promotion")
	}
	return nil
}

// replicaUp reads the agent's per-resource status record.
func (c *Controller) replicaUp(ctx context.Context, node, ns, name string, idx int) (bool, error) {
	e, err := c.St.Get(ctx, store.Key(
		"/node/"+node+"/status/resources/"+ReplicaResourceID(ns, name, idx)))
	if err != nil {
		if errors.Is(err, errors.KindNotFound) {
			return false, nil
		}
		return false, errors.Wrap(err, errors.KindInternal, "controller.RuntimePass", "replica status")
	}
	v := string(e.Value)
	return strings.Contains(v, "health=healthy") && strings.Contains(v, "in_sync=true"), nil
}

// isStatusKey reports whether k is a block status root.
func isStatusKey(k store.Key) bool {
	s := string(k)
	return len(s) >= len(StatusSuffix) && s[len(s)-len(StatusSuffix):] == StatusSuffix
}

// statusKey2ns/statusKey2name split "/blocks/<ns>/<name>".
func statusKey2ns(k store.Key) string {
	rest := strings.TrimPrefix(string(k), "/blocks/")
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[:i]
	}
	return rest
}

func statusKey2name(k store.Key) string {
	rest := strings.TrimPrefix(string(k), "/blocks/")
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[i+1:]
	}
	return ""
}
