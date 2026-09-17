// Package recovery is the §4.3 failover recovery algorithm (steps 4a–5)
// wired to the real stack: probes gather each replica's durable state
// over the T05 transport; the new primary pulls ops it is missing (4b —
// the data-loss-prevention step), brings every reachable replica to the
// same seq (4c), and marks unreachable ones Stale (4d).
//
// The divergence guard (§9) runs before ANY data movement: if two
// reachable replicas report the same sequence number with different
// CRCs, the branches have diverged and automatic recovery is refused —
// every copy is preserved untouched and a human decides
// (`expanse ctl volume diverged`). This is a deliberate, explicit code
// path, not a fallback catch-all.
package recovery

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/expanse/expanse/internal/storage/exvol/protocol"
)

// Probe is one replica's recovery-time state (step 4a). Reachable
// probes carry FetchOps, which re-reads durable op bytes from that
// replica's local copy.
type Probe struct {
	NodeID    string
	Reachable bool
	LastSeq   uint64
	// CRCs maps each durable seq to the CRC it was applied under.
	CRCs map[uint64]uint32
	// FetchOps returns ops (from, to] from this replica's durable copy.
	FetchOps func(ctx context.Context, from, to uint64) ([]protocol.WriteOp, error)
}

// Branch is one replica's version of a diverged sequence.
type Branch struct {
	NodeID string
	CRC    uint32
}

// Divergence is one sequence number with conflicting content.
type Divergence struct {
	Seq      uint64
	Branches []Branch // sorted by node ID
}

// DivergedError is returned when replicas hold the same seq with
// different CRCs (§9). Automatic recovery is refused; the caller marks
// the volume NeedsManualRecovery and preserves every copy.
type DivergedError struct {
	Divergences []Divergence
}

// NeedsManualRecovery marks the error class for callers that must not
// retry automatically.
func (e *DivergedError) NeedsManualRecovery() bool { return true }

func (e *DivergedError) Error() string {
	var b strings.Builder
	b.WriteString("divergent branches detected — manual recovery required (all copies preserved):")
	for _, d := range e.Divergences {
		fmt.Fprintf(&b, " seq=%d [", d.Seq)
		for i, br := range d.Branches {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%s=crc %#08x", br.NodeID, br.CRC)
		}
		b.WriteString("]")
	}
	return b.String()
}

// Result reports what recovery did (steps 4b–5).
type Result struct {
	// NewPrimaryID is the elected primary (step 2).
	NewPrimaryID string
	// MaxSeq is the recovery target and new vol.Sequence (step 5).
	MaxSeq uint64
	// Stale lists replicas that could not be reached (step 4d).
	Stale []string
	// Pulled counts ops applied to the new primary from other replicas
	// (step 4b — the data-loss-prevention step).
	Pulled uint64
	// Synced counts ops sent per reachable replica (step 4c).
	Synced map[string]uint64
}

// Recover runs the §4.3 failover recovery algorithm against the probed
// replicas:
//
//	4b. every op durable on a reachable replica ABOVE the new primary's
//	    lastSeq is pulled and applied locally (an op acked by 2 of 3
//	    nodes but not counted by the dying primary must not be lost);
//	4c. every other reachable replica is brought from its lastSeq to
//	    MaxSeq;
//	4d. unreachable replicas are reported Stale;
//	5.  the caller resumes at Sequence = MaxSeq.
//
// applyToPrimary applies one pulled op to the new primary's local
// copy; sendToReplica sends one op to a target replica. If either is
// nil the corresponding step is computed but skipped (planning-only
// use in tests).
func Recover(ctx context.Context, probes []Probe, applyToPrimary func(context.Context, protocol.WriteOp) error, sendToReplica func(context.Context, string, protocol.WriteOp) error) (Result, error) {
	// §9 divergence guard — before any data movement, so a diverged
	// volume is left exactly as found (all copies preserved).
	if div := detectDivergence(probes); len(div) > 0 {
		return Result{}, &DivergedError{Divergences: div}
	}

	states := make([]protocol.ReplicaState, len(probes))
	byID := map[string]*Probe{}
	for i, p := range probes {
		ops := map[uint64]protocol.WriteOp{}
		// Placeholder entries: PlanRecovery only needs to know WHICH
		// sequences a holder has (the probe's per-seq CRC map from 4a);
		// the actual bytes are fetched on demand via FetchOps below.
		for seq := range p.CRCs {
			ops[seq] = protocol.WriteOp{Seq: seq}
		}
		states[i] = protocol.ReplicaState{
			NodeID:    p.NodeID,
			LastSeq:   p.LastSeq,
			Reachable: p.Reachable,
			Ops:       ops,
		}
		byID[p.NodeID] = &probes[i]
	}
	plan := protocol.PlanRecovery(states)
	if plan.NewPrimaryID == "" {
		return Result{}, fmt.Errorf("recovery: no reachable replica to elect primary")
	}

	res := Result{
		NewPrimaryID: plan.NewPrimaryID,
		MaxSeq:       plan.MaxSeq,
		Stale:        plan.Stale,
		Synced:       map[string]uint64{},
	}

	// 4b: pull ops the new primary is missing.
	primary := byID[plan.NewPrimaryID]
	for seq, src := range plan.PullOps {
		holder := byID[src.NodeID]
		if holder == nil || holder.FetchOps == nil {
			return res, fmt.Errorf("recovery: op %d held by %s but no fetcher wired", seq, src.NodeID)
		}
		ops, err := holder.FetchOps(ctx, seq-1, seq)
		if err != nil {
			return res, fmt.Errorf("recovery: pull op %d from %s: %w", seq, src.NodeID, err)
		}
		for _, op := range ops {
			if op.Seq != seq {
				continue
			}
			if applyToPrimary != nil {
				if err := applyToPrimary(ctx, op); err != nil {
					return res, fmt.Errorf("recovery: apply pulled op %d: %w", seq, err)
				}
			}
			res.Pulled++
		}
	}

	// 4c: bring every other reachable replica to MaxSeq, sourcing ops
	// from the new primary's (now complete) durable copy.
	for target, seqs := range plan.SyncTargets {
		if len(seqs) == 0 {
			continue
		}
		from := seqs[0] - 1
		to := seqs[len(seqs)-1]
		ops, err := primary.FetchOps(ctx, from, to)
		if err != nil {
			return res, fmt.Errorf("recovery: fetch ops (%d,%d] for %s: %w", from, to, target, err)
		}
		for _, op := range ops {
			if sendToReplica != nil {
				if err := sendToReplica(ctx, target, op); err != nil {
					return res, fmt.Errorf("recovery: send op %d to %s: %w", op.Seq, target, err)
				}
			}
			res.Synced[target]++
		}
	}
	return res, nil
}

// detectDivergence finds seqs applied with different CRCs on different
// reachable replicas (§9).
func detectDivergence(probes []Probe) []Divergence {
	bySeq := map[uint64]map[string]uint32{}
	var seqs []uint64
	for _, p := range probes {
		if !p.Reachable {
			continue
		}
		for seq, crc := range p.CRCs {
			if _, ok := bySeq[seq]; !ok {
				bySeq[seq] = map[string]uint32{}
				seqs = append(seqs, seq)
			}
			bySeq[seq][p.NodeID] = crc
		}
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	var out []Divergence
	for _, seq := range seqs {
		holders := bySeq[seq]
		var first uint32
		firstSet := false
		for _, crc := range holders {
			if !firstSet {
				first, firstSet = crc, true
				continue
			}
			if crc != first {
				d := Divergence{Seq: seq}
				for node, c := range holders {
					d.Branches = append(d.Branches, Branch{NodeID: node, CRC: c})
				}
				sort.Slice(d.Branches, func(i, j int) bool { return d.Branches[i].NodeID < d.Branches[j].NodeID })
				out = append(out, d)
				break
			}
		}
	}
	return out
}
