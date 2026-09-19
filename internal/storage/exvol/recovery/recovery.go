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
	"hash/crc32"
	"sort"
	"strings"

	"github.com/expanse/expanse/internal/storage/exvol/oplog"
	"github.com/expanse/expanse/internal/storage/exvol/protocol"
)

// crc32c is the Castagnoli CRC the replication protocol uses for op
// integrity (§4.2). The probed CRCs and the fetched bytes must agree.
var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

func crc32c(b []byte) uint32 { return crc32.Checksum(b, crc32cTable) }

// Probe is one replica's recovery-time state (step 4a). Reachable
// probes carry FetchOps, which re-reads durable op bytes from that
// replica's local copy.
type Probe struct {
	NodeID    string
	Reachable bool
	LastSeq   uint64
	// CRCs maps each durable seq to the CRC it was applied under.
	CRCs map[uint64]uint32
	// Spans locates each claimed op's bytes (nil = unknown: nothing is
	// ever treated as overwritten, so CRC checks stay strict).
	Spans *oplog.Spans
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
	// SyncFailures records per-target 4c leveling failures; each
	// failed target is ALSO in Stale (it will rebuild via resync).
	SyncFailures []SyncFailure
}

// SyncFailure records one replica's failure to reach MaxSeq during 4c.
type SyncFailure struct {
	Target string
	Op     uint64 // 0 = the fetch itself failed (before any op moved)
	Err    string
}

// UnfillableError: an op that SOME reachable replica claims but NO
// reachable replica can serve honestly (re-read fails, CRC mismatch,
// conn drops mid-fetch). This is suspected data loss of claimed
// durability — the caller must NOT loop retrying; it should mark the
// volume NeedsManualRecovery and preserve all copies (§9 posture).
type UnfillableError struct {
	Seq uint64
	Err error
}

func (e *UnfillableError) Error() string {
	return fmt.Sprintf("recovery: op %d claimed but no holder can serve it honestly: %v", e.Seq, e.Err)
}

func (e *UnfillableError) Unwrap() error { return e.Err }

// Recover runs the §4.3 failover recovery algorithm against the probed
// replicas:
//
//	4b. every op durable on a reachable replica ABOVE the new primary's
//	    lastSeq is pulled and applied locally (an op acked by 2 of 3
//	    nodes but not counted by the dying primary must not be lost);
//	4c. every reachable replica is leveled to MaxSeq from the CALLER's
//	    own (now complete) durable copy — SelfFetch. Sourcing from the
//	    caller (not from the plan's elected node) matters: the caller is
//	    the node actually serving, its copy was just filled to MaxSeq,
//	    and a peer's claims may be unserveable (torn zvol behind honest
//	    oplog claims);
//	4d. unreachable replicas are reported Stale;
//	5.  the caller resumes at Sequence = MaxSeq.
//
// applyToPrimary applies one pulled op to the new primary's local
// copy; sendToReplica sends one op to a target replica. If either is
// nil the corresponding step is computed but skipped (planning-only
// use in tests).
func Recover(ctx context.Context, probes []Probe, selfFetch func(ctx context.Context, from, to uint64) ([]protocol.WriteOp, error), applyToPrimary func(context.Context, protocol.WriteOp) error, sendToReplica func(context.Context, string, protocol.WriteOp) error) (Result, error) {
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

	// 4b: pull ops the new primary is missing. HONESTY RULE: a holder
	// that fails to serve an op it CLAIMS to hold (re-read error, CRC
	// mismatch, connection drop mid-fetch) is a dishonest holder — it
	// must be excluded from sourcing, not treated as unreachable-network
	// (the §4.3 4b flap). Try every reachable holder claiming the seq
	// (divergence was already ruled out above, so any holder serves the
	// same branch); only fail when NO holder can serve it honestly.
	for seq, src := range plan.PullOps {
		op, err := fetchHonest(ctx, probes, byID, seq, src.NodeID)
		if err != nil {
			return res, &UnfillableError{Seq: seq, Err: err}
		}
		if applyToPrimary != nil {
			if err := applyToPrimary(ctx, op); err != nil {
				return res, fmt.Errorf("recovery: apply pulled op %d: %w", seq, err)
			}
		}
		res.Pulled++
	}

	// 4c: level EVERY reachable replica to MaxSeq from the caller's own
	// durable copy (idempotent on the targets: duplicates re-ACK). A
	// target that cannot be leveled is marked STALE (step 4d) and
	// skipped — failing the whole recovery made every failover retry
	// loop forever on the same broken replica (the VM flap: "fetch ops
	// (0,4] for n2: EOF" three times, then elect the WRONG node).
	// Leveling the plan-elected peer too (when it differs from the
	// caller) is deliberate: its claims can be unserveable, and the
	// resends rebuild the bytes it claims to hold.
	for i := range probes {
		target := probes[i]
		if !target.Reachable {
			continue
		}
		if selfFetch == nil {
			break // planning-only use
		}
		if err := levelOne(ctx, target, res.MaxSeq, selfFetch, probes, byID, sendToReplica); err != nil {
			res.Stale = append(res.Stale, target.NodeID)
			res.SyncFailures = append(res.SyncFailures, SyncFailure{Target: target.NodeID, Err: err.Error()})
			continue
		}
		res.Synced[target.NodeID]++
	}
	return res, nil
}

// levelOne brings one replica to MaxSeq. Its CLAIMS (oplog) may lie —
// a torn zvol behind honest oplog records (the §4.3 4a/4b gap) must be
// caught, so the target's durability is SAMPLE-VERIFIED first: the
// lowest and highest ops it CURRENTLY CLAIMS (its own CRCs map — not
// literal seq 1, which a full resync can long since have rotated out
// of every node's retained history) are re-read via its own FetchOps.
// A failed sample means the claims are unserveable → the FULL op range
// is resent from the caller's copy (the resends rebuild the missing
// bytes; duplicates re-ACK idempotently on a healthy replica).
func levelOne(ctx context.Context, target Probe, maxSeq uint64, selfFetch func(context.Context, uint64, uint64) ([]protocol.WriteOp, error), probes []Probe, byID map[string]*Probe, sendToReplica func(context.Context, string, protocol.WriteOp) error) error {
	claimsCurrent := target.LastSeq >= maxSeq
	if claimsCurrent && target.FetchOps != nil && maxSeq > 0 {
		sampleSeqs := []uint64{maxSeq}
		if lo := minClaimedSeq(target.CRCs); lo > 0 && lo != maxSeq {
			sampleSeqs = append(sampleSeqs, lo)
		}
		for _, seq := range sampleSeqs {
			ops, err := target.FetchOps(ctx, seq-1, seq)
			if err != nil {
				claimsCurrent = false // unserveable claims
				break
			}
			for _, op := range ops {
				if op.Seq != seq || op.Flush {
					continue
				}
				// The bytes must match the target's OWN claim — a
				// truncated zvol reads back zeros without error.
				if _, _, err := target.vouch(op); err != nil {
					claimsCurrent = false
				}
			}
			if !claimsCurrent {
				break
			}
		}
	}
	if claimsCurrent {
		return nil // verified current; nothing to send
	}
	from := uint64(0)
	if !claimsCurrent && target.LastSeq < maxSeq {
		// Claims below MaxSeq: resend the claimed tail… unless the
		// sample above already failed, in which case from 0.
		from = target.LastSeq
	}
	for seq := from + 1; seq <= maxSeq; seq++ {
		op, err := fetchOne(ctx, seq, selfFetch, probes, byID)
		if err != nil {
			return err
		}
		if sendToReplica == nil {
			continue
		}
		if err := sendToReplica(ctx, target.NodeID, op); err != nil {
			return fmt.Errorf("send op %d: %w", op.Seq, err)
		}
	}
	return nil
}

// minClaimedSeq returns the lowest seq a replica's CRCs map claims to
// hold, or 0 if it claims none. A node's oplog is reset on every full
// resync (§4.3 resync), so seq 1 is routinely NOT among a healthy,
// fully-current replica's claims once a volume has rotated through
// even one full resend — that is expected, not evidence of a torn
// zvol, and must not be sampled as if it were.
func minClaimedSeq(crcs map[uint64]uint32) uint64 {
	var lo uint64
	for seq := range crcs {
		if lo == 0 || seq < lo {
			lo = seq
		}
	}
	return lo
}

// fetchOne sources one op for leveling: the caller's own durable copy
// first, then any honest holder (the caller's claims registry can lag
// its writer — ops filled by 4b pulls are durable but unlogged).
func fetchOne(ctx context.Context, seq uint64, selfFetch func(context.Context, uint64, uint64) ([]protocol.WriteOp, error), probes []Probe, byID map[string]*Probe) (protocol.WriteOp, error) {
	if selfFetch != nil {
		if ops, err := selfFetch(ctx, seq-1, seq); err == nil {
			for _, op := range ops {
				if op.Seq == seq {
					return op, nil
				}
			}
		}
	}
	op, err := fetchHonest(ctx, probes, byID, seq, "")
	if err != nil {
		return protocol.WriteOp{}, fmt.Errorf("no honest source for op %d: %w", seq, err)
	}
	return op, nil
}

// fetchHonest sources one op from among the reachable replicas that
// claim to hold it, skipping holders that fail (re-read error, conn
// drop). It verifies the fetched bytes against the holder's own probed
// CRC (recovery 4a) — a holder serving bytes that do not match its own
// claim is dishonest and is skipped too.
func fetchHonest(ctx context.Context, probes []Probe, byID map[string]*Probe, seq uint64, preferred string) (protocol.WriteOp, error) {
	var firstErr error
	try := func(id string) (protocol.WriteOp, bool, error) {
		h := byID[id]
		if h == nil || h.FetchOps == nil || !h.Reachable {
			return protocol.WriteOp{}, false, fmt.Errorf("no fetcher for %s", id)
		}
		ops, err := h.FetchOps(ctx, seq-1, seq)
		if err != nil {
			return protocol.WriteOp{}, false, err
		}
		for _, op := range ops {
			if op.Seq != seq {
				continue
			}
			return h.vouch(op)
		}
		return protocol.WriteOp{}, false, fmt.Errorf("%s served no op %d", id, seq)
	}
	if op, _, err := try(preferred); err == nil {
		return op, nil
	} else {
		firstErr = err
	}
	// Preferred holder is dishonest — try every other claimant.
	for i := range probes {
		p := &probes[i]
		if p.NodeID == preferred || !p.Reachable {
			continue
		}
		if _, claims := p.CRCs[seq]; !claims {
			continue
		}
		if op, _, err := try(p.NodeID); err == nil {
			return op, nil
		} else {
			firstErr = fmt.Errorf("%w; %s: %v", firstErr, p.NodeID, err)
		}
	}
	return protocol.WriteOp{}, firstErr
}

// vouch verifies a fetched op against this holder's own claim. A claim's
// CRC covers the payload as first written, so an op whose range a later
// op rewrote can only be checked by that later op: its current bytes are
// served under a fresh CRC (the wire still validates them).
func (p *Probe) vouch(op protocol.WriteOp) (protocol.WriteOp, bool, error) {
	claim, claimed := p.CRCs[op.Seq]
	if op.Flush || !claimed {
		return op, true, nil
	}
	if p.Spans.Superseded(op.Seq) {
		op.CRC = crc32c(op.Data)
		return op, true, nil
	}
	if crc32c(op.Data) != claim {
		return protocol.WriteOp{}, false, fmt.Errorf("bytes from %s fail crc32c (claim %08x)", p.NodeID, claim)
	}
	return op, true, nil
}

// FillOne sources one op for the CALLER's local fill (the mirror of 4b
// used when the caller is not the plan-elected node): every reachable
// holder claiming the seq is tried, bytes are CRC-verified against the
// holder's own claim, and only an honest serve is accepted. Never
// returns fabricated bytes.
func FillOne(ctx context.Context, probes []Probe, seq uint64) (protocol.WriteOp, error) {
	byID := map[string]*Probe{}
	for i := range probes {
		byID[probes[i].NodeID] = &probes[i]
	}
	var preferred string
	for i := range probes {
		if probes[i].Reachable {
			if _, ok := probes[i].CRCs[seq]; ok {
				preferred = probes[i].NodeID
				break
			}
		}
	}
	return fetchHonest(ctx, probes, byID, seq, preferred)
}

// detectDivergence finds seqs applied with different CRCs on different
// reachable replicas (§9).
func detectDivergence(probes []Probe) []Divergence {
	bySeq := map[uint64]map[string]uint32{}
	var seqs []uint64
	overwritten := map[uint64]bool{}
	for _, p := range probes {
		for seq := range p.CRCs {
			if p.Spans.Superseded(seq) {
				overwritten[seq] = true
			}
		}
	}
	for _, p := range probes {
		if !p.Reachable {
			continue
		}
		for seq, crc := range p.CRCs {
			if overwritten[seq] {
				continue // re-recorded on leveling; the op that overwrote it is compared instead
			}
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
