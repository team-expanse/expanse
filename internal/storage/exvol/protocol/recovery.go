package protocol

import (
	"sort"
)

// ReplicaState is a failover-time snapshot of one replica (§4.3
// failover steps 2–4).
type ReplicaState struct {
	NodeID string
	// LastSeq is the replica's last contiguously applied seq.
	LastSeq uint64
	// Reachable: false → the replica is marked Stale (step 4d).
	Reachable bool
	// Ops are the write ops this replica holds durable, keyed by seq.
	// A healthy secondary holds ops it applied; the new primary holds
	// ops it applied locally. Step 4b pulls from these.
	Ops map[uint64]WriteOp
}

// RecoveryPlan is the output of the failover recovery algorithm (§4.3
// steps 4a–4d, 5).
type RecoveryPlan struct {
	// NewPrimaryID is the elected primary (highest lastSeq among
	// reachable replicas, ties by lowest node ID — step 2).
	NewPrimaryID string
	// MaxSeq is max(all reachable lastSeqs) — the recovery target and
	// the new vol.Sequence (step 5).
	MaxSeq uint64
	// PullOps are ops the NEW PRIMARY itself is missing and must fetch
	// from another replica before it may serve writes (step 4b — the
	// data-loss prevention rule). Keyed by seq; Source is where to pull
	// each from.
	PullOps map[uint64]PullSource
	// SyncTargets maps each reachable replica (excluding the new
	// primary) to the ordered seqs it must receive to reach MaxSeq
	// (step 4c: bring all reachable replicas to the same seq).
	SyncTargets map[string][]uint64
	// Stale lists replicas that cannot be reached (step 4d).
	Stale []string
}

// PullSource names the replica holding a given op.
type PullSource struct {
	NodeID string
	Op     WriteOp
}

// ElectPrimary implements §4.3 failover step 2: among reachable
// replicas, the one with the HIGHEST lastSeq wins; ties break by lowest
// node ID.
func ElectPrimary(reps []ReplicaState) (string, error) {
	var best *ReplicaState
	for i := range reps {
		r := &reps[i]
		if !r.Reachable {
			continue
		}
		if best == nil ||
			r.LastSeq > best.LastSeq ||
			(r.LastSeq == best.LastSeq && r.NodeID < best.NodeID) {
			best = r
		}
	}
	if best == nil {
		return "", errNoCandidate("no reachable replica for primary election")
	}
	return best.NodeID, nil
}

type errNoCandidate string

func (e errNoCandidate) Error() string { return string(e) }

// PlanRecovery computes steps 4a–5 of the failover algorithm:
//
//	a. inputs are the reachable lastSeqs;
//	b. ops durable on any replica at a HIGHER seq than the new primary's
//	   lastSeq are pulled back into it (4b — without this, an op acked
//	   by 2 of 3 replicas but not counted by the dying primary is lost);
//	c. every reachable replica is brought to MaxSeq via targeted sends;
//	d. unreachable replicas are marked Stale;
//	5. the new primary resumes at vol.Sequence = MaxSeq.
func PlanRecovery(reps []ReplicaState) RecoveryPlan {
	primary, err := ElectPrimary(reps)
	if err != nil {
		// Caller handles empty candidate sets; return an empty plan.
		return RecoveryPlan{}
	}

	var newPrimary *ReplicaState
	maxSeq := uint64(0)
	for i := range reps {
		r := &reps[i]
		if r.NodeID == primary {
			newPrimary = r
		}
		if r.Reachable && r.LastSeq > maxSeq {
			maxSeq = r.LastSeq
		}
	}

	plan := RecoveryPlan{
		NewPrimaryID: primary,
		MaxSeq:       maxSeq,
		PullOps:      map[uint64]PullSource{},
		SyncTargets:  map[string][]uint64{},
	}

	// 4b: ops above the new primary's lastSeq that any reachable replica
	// holds. Sources are scanned lowest-node-ID-first for determinism.
	var holders []*ReplicaState
	for i := range reps {
		if reps[i].Reachable && reps[i].NodeID != primary {
			holders = append(holders, &reps[i])
		}
	}
	sort.Slice(holders, func(i, j int) bool { return holders[i].NodeID < holders[j].NodeID })

	if newPrimary.LastSeq < maxSeq {
		for seq := newPrimary.LastSeq + 1; seq <= maxSeq; seq++ {
			for _, h := range holders {
				if op, ok := h.Ops[seq]; ok {
					plan.PullOps[seq] = PullSource{NodeID: h.NodeID, Op: op}
					break
				}
			}
		}
	}

	// 4c: bring every other reachable replica from its lastSeq to maxSeq.
	for i := range reps {
		r := &reps[i]
		if !r.Reachable || r.NodeID == primary {
			continue
		}
		if r.LastSeq < maxSeq {
			missing := make([]uint64, 0, maxSeq-r.LastSeq)
			for seq := r.LastSeq + 1; seq <= maxSeq; seq++ {
				missing = append(missing, seq)
			}
			plan.SyncTargets[r.NodeID] = missing
		}
	}

	// 4d: unreachable replicas are Stale. All reachable replicas keep
	// every copy — divergence is never auto-resolved (NeedsManualRecovery
	// is the controller's call, not the protocol's).
	for i := range reps {
		if !reps[i].Reachable {
			plan.Stale = append(plan.Stale, reps[i].NodeID)
		}
	}
	sort.Strings(plan.Stale)

	return plan
}
