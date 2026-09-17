package protocol

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"
)

// makeOp builds a valid WriteOp with correct CRC.
func makeOp(seq uint64, offset int, data []byte) WriteOp {
	return WriteOp{Seq: seq, Offset: uint64(offset), Data: data, CRC: CRC32C(data)}
}

// deliverAll sends op to every secondary, returning the number of ACKs.
func deliverAll(secs []*Secondary, op WriteOp) int {
	acks := 0
	for _, s := range secs {
		if r := s.Handle(op); r.ACK {
			acks++
		}
	}
	return acks
}

// --- Sequence assignment (R2) ---

func TestSequenceAssignmentMonotonicGapless(t *testing.T) {
	p := NewPrimary("vol-abc", 3)
	for want := uint64(1); want <= 1000; want++ {
		got, err := p.NextSeq()
		if err != nil {
			t.Fatalf("NextSeq at %d: %v", want, err)
		}
		if got != want {
			t.Fatalf("seq = %d, want %d (must be monotonic gapless)", got, want)
		}
	}
}

func TestSequenceAssignmentAfterLeaseLossFailsEIO(t *testing.T) {
	p := NewPrimary("vol-abc", 3)
	if _, err := p.NextSeq(); err != nil {
		t.Fatal(err)
	}
	p.HasLease = false // R5: lease lost
	for i := 0; i < 10; i++ {
		if _, err := p.NextSeq(); err != ErrLeaseLost {
			t.Fatalf("write after lease loss: err = %v, want ErrLeaseLost (EIO, never hang)", err)
		}
	}
}

// --- Quorum math (R1, R6) ---

func TestQuorumMathR1toR5(t *testing.T) {
	for r := 1; r <= 5; r++ {
		p := NewPrimary("vol-abc", r)
		want := r/2 + 1
		if got := p.Quorum(); got != want {
			t.Errorf("R=%d: quorum = %d, want floor(R/2)+1 = %d", r, got, want)
		}
		// Ack requires exactly quorum: quorum-1 must not ack, quorum must.
		if p.ShouldAck(want - 1) {
			t.Errorf("R=%d: acked at %d durability, want reject (R1: never ack before quorum)", r, want-1)
		}
		if !p.ShouldAck(want) {
			t.Errorf("R=%d: did not ack at %d durability", r, want)
		}
		if !p.ShouldAck(want + 1) {
			t.Errorf("R=%d: did not ack at %d durability", r, want+1)
		}
	}
}

func TestQuorumR2RequiresBothReplicas(t *testing.T) {
	// R6: R=2 quorum is 2 — both replicas, deliberately.
	p := NewPrimary("vol-abc", 2)
	if p.Quorum() != 2 {
		t.Fatalf("R=2 quorum = %d, want 2", p.Quorum())
	}
	if p.ShouldAck(1) {
		t.Error("R=2 acked with only 1 durable — violates R6")
	}
}

// --- Out-of-order buffering (R3) ---

func TestOutOfOrderBufferingShuffledDelivery(t *testing.T) {
	const (
		size   = 4096
		numOps = 64
		opSize = 100
	)
	ref := make([]byte, size)
	sec := NewSecondary("n1", size)

	ops := make([]WriteOp, numOps)
	for i := 0; i < numOps; i++ {
		data := bytes.Repeat([]byte{byte(i + 1)}, opSize)
		off := (i * opSize) % (size - opSize)
		ops[i] = makeOp(uint64(i+1), off, data)
		copy(ref[off:], data) // reference: applied in order
	}

	shuffled := make([]WriteOp, len(ops))
	copy(shuffled, ops)
	rng := rand.New(rand.NewSource(42))
	for i := len(shuffled) - 1; i > 0; i-- {
		j := rng.Intn(i + 1)
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	}

	// Every op delivered exactly once, in shuffled order. The FIRST
	// replies NACK (gap), the last must ACK the full range.
	for i, op := range shuffled {
		r := sec.Handle(op)
		if i == len(shuffled)-1 {
			if !r.ACK || r.Seq != numOps {
				t.Fatalf("last delivered op: reply = %+v, want ACK seq=%d (in-order drain)", r, numOps)
			}
		}
	}

	if sec.LastSeq() != numOps {
		t.Errorf("lastSeq = %d, want %d", sec.LastSeq(), numOps)
	}
	if !bytes.Equal(sec.Data, ref) {
		t.Error("content diverged from in-order reference after shuffled delivery")
	}
	if len(sec.pending) != 0 {
		t.Errorf("pending buffer not drained: %d ops left", len(sec.pending))
	}
}

func TestOutOfOrderDuplicateDeliveryIdempotent(t *testing.T) {
	sec := NewSecondary("n1", 1024)
	op := makeOp(1, 0, bytes.Repeat([]byte{7}, 64))
	sec.Handle(op)
	// Network duplicates the same op: idempotent re-ACK, no double apply.
	r := sec.Handle(op)
	if !r.ACK {
		t.Fatalf("duplicate op: reply = %+v, want ACK", r)
	}
	if sec.LastSeq() != 1 {
		t.Errorf("lastSeq = %d, want 1", sec.LastSeq())
	}
	// And a genuinely conflicting duplicate (same seq, different bytes)
	// must not be applied twice / regress lastSeq.
	conflict := makeOp(1, 0, bytes.Repeat([]byte{9}, 64))
	if r := sec.Handle(conflict); !r.ACK {
		t.Logf("conflicting duplicate reply: %+v", r)
	}
	if sec.LastSeq() != 1 {
		t.Errorf("lastSeq regressed to %d", sec.LastSeq())
	}
}

// --- CRC (NACK → retransmit) ---

func TestCRCMismatchNACKThenRetransmit(t *testing.T) {
	sec := NewSecondary("n1", 1024)
	data := bytes.Repeat([]byte{3}, 100)

	bad := makeOp(1, 0, data)
	bad.CRC ^= 0xFF // corrupt the wire CRC
	r := sec.Handle(bad)
	if r.ACK || !r.Retransmit {
		t.Fatalf("corrupt op: reply = %+v, want NACK+Retransmit", r)
	}
	if sec.LastSeq() != 0 {
		t.Fatalf("corrupt op applied! lastSeq = %d", sec.LastSeq())
	}
	if bytes.Contains(sec.Data[:100], data) {
		t.Fatal("corrupt data reached the store")
	}

	// Retransmit with correct CRC → ACK, applied exactly once.
	r = sec.Handle(makeOp(1, 0, data))
	if !r.ACK || r.Seq != 1 {
		t.Fatalf("retransmit: reply = %+v, want ACK seq=1", r)
	}
	if !bytes.Equal(sec.Data[:100], data) {
		t.Error("retransmitted data not applied correctly")
	}
}

// --- Gap detection → resync ---

func TestGapNACKReportsLastSeq(t *testing.T) {
	sec := NewSecondary("n1", 1024)
	sec.Handle(makeOp(1, 0, bytes.Repeat([]byte{1}, 32)))

	op5 := makeOp(5, 0, bytes.Repeat([]byte{2}, 32))
	r := sec.Handle(op5)
	if r.ACK {
		t.Fatal("gap op acked — violates R2 ordering")
	}
	if r.LastSeq != 1 {
		t.Errorf("NACK lastSeq = %d, want 1 (primary learns the gap)", r.LastSeq)
	}
	// After the hole (2,3,4) fills, 5 drains automatically.
	for seq := uint64(2); seq <= 4; seq++ {
		sec.Handle(makeOp(seq, 0, bytes.Repeat([]byte{byte(seq)}, 32)))
	}
	if sec.LastSeq() != 5 {
		t.Errorf("lastSeq = %d, want 5 after drain", sec.LastSeq())
	}
}

func TestWindowExceededTriggersResyncNotSilentDrop(t *testing.T) {
	// Small window: 4 ops / 1 KiB. Deliver ops 2..6 with seq 1 never
	// arriving: 4 buffer, the 5th exceeds the window → Resync NACK.
	sec := newSecondaryWindow("n1", 4096, 4, 1024)

	var last Reply
	for seq := uint64(2); seq <= 6; seq++ {
		last = sec.Handle(makeOp(seq, 0, bytes.Repeat([]byte{byte(seq)}, 100)))
	}
	if last.ACK {
		t.Fatal("window-exceeded op acked")
	}
	if !last.Resync {
		t.Fatalf("reply = %+v, want Resync trigger (never a silent drop)", last)
	}
	if !sec.ResyncNeeded {
		t.Error("ResyncNeeded not latched")
	}
	// Subsequent ops keep NACKing resync until the controller resets.
	r := sec.Handle(makeOp(7, 0, bytes.Repeat([]byte{7}, 100)))
	if !r.Resync {
		t.Errorf("post-window op: reply = %+v, want Resync", r)
	}
	if len(sec.pending) != 4 {
		t.Errorf("ops dropped silently: %d pending, want 4 buffered until resync", len(sec.pending))
	}
}

func TestWindowBytesExceededTriggersResync(t *testing.T) {
	// 2-op window, 100-byte cap: big ops exceed the byte window.
	sec := newSecondaryWindow("n1", 8192, 2, 100)
	sec.Handle(makeOp(2, 0, bytes.Repeat([]byte{1}, 60)))
	sec.Handle(makeOp(3, 0, bytes.Repeat([]byte{1}, 60)))
	r := sec.Handle(makeOp(4, 0, bytes.Repeat([]byte{1}, 60)))
	if !r.Resync {
		t.Fatalf("reply = %+v, want Resync on byte-window exceed", r)
	}
}

// --- Convergence property ---

func TestConvergencePropertyRandomInterleavedWrites(t *testing.T) {
	const (
		size   = 8192
		numOps = 200
	)
	for seed := int64(1); seed <= 10; seed++ {
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			secs := []*Secondary{
				NewSecondary("n1", size),
				NewSecondary("n2", size),
				NewSecondary("n3", size),
			}
			// Reference: ops applied in seq order to a pristine store.
			ref := make([]byte, size)

			for seq := uint64(1); seq <= numOps; seq++ {
				n := 1 + rng.Intn(200)
				off := rng.Intn(size - n)
				data := make([]byte, n)
				rng.Read(data)

				op := makeOp(seq, off, data)
				// Reference application.
				copy(ref[off:], data)

				// Random per-replica delivery orders + duplicates.
				for _, s := range secs {
					deliveries := rng.Intn(3) + 1 // 1..3 deliveries (dupes)
					for d := 0; d < deliveries; d++ {
						s.Handle(op)
					}
				}
			}

			for _, s := range secs {
				if s.LastSeq() != numOps {
					t.Errorf("%s: lastSeq = %d, want %d", s.ID, s.LastSeq(), numOps)
				}
				if !bytes.Equal(s.Data, ref) {
					t.Errorf("%s: content diverged from reference", s.ID)
				}
			}
			// All replicas identical to each other.
			for _, s := range secs[1:] {
				if !bytes.Equal(s.Data, secs[0].Data) {
					t.Errorf("%s diverged from n1", s.ID)
				}
			}
		})
	}
}

// --- Failover: election + recovery, including the spec's exact 10/12/11
// scenario (recovery step 4b) ---

func TestElectPrimaryHighestSeqTieLowestID(t *testing.T) {
	reps := []ReplicaState{
		{NodeID: "n3", LastSeq: 11, Reachable: true, Ops: map[uint64]WriteOp{}},
		{NodeID: "n2", LastSeq: 12, Reachable: true, Ops: map[uint64]WriteOp{}},
		{NodeID: "n1", LastSeq: 10, Reachable: true, Ops: map[uint64]WriteOp{}},
	}
	id, err := ElectPrimary(reps)
	if err != nil {
		t.Fatal(err)
	}
	if id != "n2" {
		t.Errorf("elected %q, want n2 (highest lastSeq)", id)
	}

	// Tie: n1 and n2 both at 12 → lowest node ID wins.
	reps[1].LastSeq = 12
	reps[2].LastSeq = 12
	reps[0].LastSeq = 12
	// relabel: n1 has 12, n2 has 12, n3 has 11
	reps[0] = ReplicaState{NodeID: "n1", LastSeq: 12, Reachable: true, Ops: map[uint64]WriteOp{}}
	reps[1] = ReplicaState{NodeID: "n2", LastSeq: 12, Reachable: true, Ops: map[uint64]WriteOp{}}
	reps[2] = ReplicaState{NodeID: "n3", LastSeq: 11, Reachable: true, Ops: map[uint64]WriteOp{}}
	id, _ = ElectPrimary(reps)
	if id != "n1" {
		t.Errorf("tie elected %q, want n1 (lowest node ID)", id)
	}
}

func TestSpecRecoveryScenario10_12_11(t *testing.T) {
	// The spec's own example (§4.3 failover, after 4b): replicas at seq
	// 10, 12, 11 → new primary is the seq-12 one, and 10 and 11 are
	// brought to 12 before declaring Healthy.
	op := func(seq uint64) WriteOp {
		return makeOp(seq, 0, bytes.Repeat([]byte{byte(seq)}, 32))
	}
	reps := []ReplicaState{
		{NodeID: "n1", LastSeq: 10, Reachable: true, Ops: ops1toN(op, 10)},
		{NodeID: "n2", LastSeq: 12, Reachable: true, Ops: ops1toN(op, 12)},
		{NodeID: "n3", LastSeq: 11, Reachable: true, Ops: ops1toN(op, 11)},
	}

	plan := PlanRecovery(reps)

	if plan.NewPrimaryID != "n2" {
		t.Errorf("new primary = %q, want n2 (seq 12)", plan.NewPrimaryID)
	}
	if plan.MaxSeq != 12 {
		t.Errorf("MaxSeq = %d, want 12", plan.MaxSeq)
	}

	// n1 (10) must be brought to 12: missing 11, 12.
	if got := fmt.Sprint(plan.SyncTargets["n1"]); got != "[11 12]" {
		t.Errorf("n1 sync target = %s, want [11 12]", got)
	}
	// n3 (11) must be brought to 12: missing 12.
	if got := fmt.Sprint(plan.SyncTargets["n3"]); got != "[12]" {
		t.Errorf("n3 sync target = %s, want [12]", got)
	}
	// The new primary itself already has everything: nothing to pull.
	if len(plan.PullOps) != 0 {
		t.Errorf("PullOps = %v, want empty", plan.PullOps)
	}
	if len(plan.Stale) != 0 {
		t.Errorf("Stale = %v, want empty (all reachable)", plan.Stale)
	}

	// Execute the plan through real secondaries: all reach 12, identical
	// content.
	mk := func(last uint64) *Secondary {
		s := NewSecondary("x", 1024)
		for seq := uint64(1); seq <= last; seq++ {
			s.Handle(op(seq))
		}
		return s
	}
	n1 := mk(10)
	for _, seq := range plan.SyncTargets["n1"] {
		if r := n1.Handle(op(seq)); !r.ACK {
			t.Fatalf("n1 sync op %d: %+v", seq, r)
		}
	}
	if n1.LastSeq() != 12 {
		t.Errorf("n1 lastSeq = %d, want 12 (brought to 12 before Healthy)", n1.LastSeq())
	}
}

func ops1toN(op func(uint64) WriteOp, n uint64) map[uint64]WriteOp {
	m := map[uint64]WriteOp{}
	for seq := uint64(1); seq <= n; seq++ {
		m[seq] = op(seq)
	}
	return m
}

func TestRecoveryStep4bPullOpsFromHigherSeqReplica(t *testing.T) {
	// The data-loss scenario 4b exists for: the dying primary counted
	// lastSeq=10, but ops 11 and 12 were durable on secondary n2 (it
	// acked them; the primary hadn't counted quorum yet). The new
	// primary MUST pull 11 and 12 back before serving writes.
	op := func(seq uint64) WriteOp {
		return makeOp(seq, 0, bytes.Repeat([]byte{byte(seq)}, 32))
	}
	reps := []ReplicaState{
		{NodeID: "n1", LastSeq: 10, Reachable: true, Ops: ops1toN(op, 10)},
		{NodeID: "n2", LastSeq: 12, Reachable: true, Ops: ops1toN(op, 12)},
		{NodeID: "n3", LastSeq: 10, Reachable: true, Ops: ops1toN(op, 10)},
	}
	// n1 has the highest seq among... no: n2 does, so n2 becomes primary.
	// Force the 4b case: make n1 the new primary by giving IT 10 while
	// n2 has 12 — n2 wins election. Swap roles: new primary must be the
	// highest — to exercise 4b, the new primary needs a LOWER seq than
	// some other replica, which the election forbids. 4b therefore fires
	// when the primary's own Ops map lacks ops its lastSeq implies it
	// has (old primary's local copy vs acked secondaries). Model the
	// canonical case: new primary at 11, another replica at 12.
	reps[0] = ReplicaState{NodeID: "n1", LastSeq: 11, Reachable: true, Ops: ops1toN(op, 11)}
	reps[1] = ReplicaState{NodeID: "n2", LastSeq: 12, Reachable: true, Ops: ops1toN(op, 12)}
	reps[2] = ReplicaState{NodeID: "n3", LastSeq: 10, Reachable: true, Ops: ops1toN(op, 10)}

	plan := PlanRecovery(reps)
	if plan.NewPrimaryID != "n2" || plan.MaxSeq != 12 {
		t.Fatalf("primary = %q max = %d, want n2/12", plan.NewPrimaryID, plan.MaxSeq)
	}

	// n3 must be brought from 10 to 12.
	if got := fmt.Sprint(plan.SyncTargets["n3"]); got != "[11 12]" {
		t.Errorf("n3 sync target = %s, want [11 12]", got)
	}
	// n1 (the old-primary-side replica at 11) gets 12.
	if got := fmt.Sprint(plan.SyncTargets["n1"]); got != "[12]" {
		t.Errorf("n1 sync target = %s, want [12]", got)
	}
}

func TestRecoveryUnreachableMarkedStaleKeptNotDeleted(t *testing.T) {
	op := func(seq uint64) WriteOp {
		return makeOp(seq, 0, bytes.Repeat([]byte{byte(seq)}, 32))
	}
	reps := []ReplicaState{
		{NodeID: "n1", LastSeq: 12, Reachable: true, Ops: ops1toN(op, 12)},
		{NodeID: "n2", LastSeq: 9, Reachable: false, Ops: ops1toN(op, 9)},
		{NodeID: "n3", LastSeq: 12, Reachable: true, Ops: ops1toN(op, 12)},
	}
	plan := PlanRecovery(reps)
	if plan.NewPrimaryID != "n1" {
		t.Errorf("primary = %q, want n1 (tie with n3, lowest ID)", plan.NewPrimaryID)
	}
	if fmt.Sprint(plan.Stale) != "[n2]" {
		t.Errorf("Stale = %v, want [n2]", plan.Stale)
	}
	// The stale replica's state is preserved in the input — the plan
	// never says "delete" or "reallocate"; resync (T12) reconciles it.
	if len(plan.SyncTargets) != 0 {
		t.Errorf("SyncTargets = %v, want empty (n2 unreachable, not synced)", plan.SyncTargets)
	}
}

func TestRecoveryNoReachableReplicas(t *testing.T) {
	reps := []ReplicaState{
		{NodeID: "n1", LastSeq: 5, Reachable: false, Ops: map[uint64]WriteOp{}},
	}
	if _, err := ElectPrimary(reps); err == nil {
		t.Error("election succeeded with zero reachable replicas")
	}
	plan := PlanRecovery(reps)
	if plan.NewPrimaryID != "" {
		t.Errorf("plan primary = %q, want empty", plan.NewPrimaryID)
	}
}

// --- Full write-path walkthrough (steps 1–8) ---

func TestWritePathSteps1to8(t *testing.T) {
	const R = 3
	p := NewPrimary("vol-abc", R)
	local := NewSecondary("n1", 4096)
	secs := []*Secondary{NewSecondary("n2", 4096), NewSecondary("n3", 4096)}

	for seq := uint64(1); seq <= 20; seq++ {
		// 1. lease check + 2. assign seq.
		s, err := p.NextSeq()
		if err != nil {
			t.Fatal(err)
		}
		if s != seq {
			t.Fatalf("seq = %d, want %d", s, seq)
		}
		op := makeOp(s, int(seq-1)*16, bytes.Repeat([]byte{byte(seq)}, 16))

		// 4. primary local write (O_DIRECT|O_DSYNC in production).
		local.Handle(op)

		// 5+6. send to secondaries in parallel; count durable (self + acks).
		durable := 1 + deliverAll(secs, op)

		// 7. ack only at quorum (R1). R=3: quorum 2.
		if p.ShouldAck(durable) {
			// 8. record lastAcked.
			p.RecordAcked(s)
		} else {
			t.Fatalf("seq %d: durable=%d below quorum=%d", s, durable, p.Quorum())
		}
	}

	if p.LastAcked() != 20 {
		t.Errorf("lastAcked = %d, want 20", p.LastAcked())
	}
	// All replicas converged.
	for _, s := range append(secs, local) {
		if s.LastSeq() != 20 {
			t.Errorf("%s lastSeq = %d, want 20", s.ID, s.LastSeq())
		}
	}
	for _, s := range secs[0:] {
		if !bytes.Equal(s.Data, local.Data) {
			t.Errorf("%s diverged from primary", s.ID)
		}
	}
}

func TestWritePathQuorumLostNoAck(t *testing.T) {
	const R = 3
	p := NewPrimary("vol-abc", R)
	local := NewSecondary("n1", 1024)
	up := NewSecondary("n2", 1024)
	// n3 partitioned: only 2 of 3 reachable, but quorum for R=3 is 2 —
	// still acks. Now lose n2 too: 1 durable < 2 → no ack (R1).
	s, err := p.NextSeq()
	if err != nil {
		t.Fatal(err)
	}
	op := makeOp(s, 0, bytes.Repeat([]byte{1}, 32))
	local.Handle(op)
	durable := 1 // only local; no secondary acks
	if p.ShouldAck(durable) {
		t.Fatal("acked below quorum — R1 violated")
	}
	_ = up
}

// --- Resync replay (post-resync convergence, §4.3 resync step 5) ---

func TestResyncReplayMarksSecondaryAndConverges(t *testing.T) {
	// A stale replica is resynced by replaying missing ops; on reaching
	// the current seq it rejoins quorum as Secondary.
	// Simulate the window-exceeded stale state (window of 1 op: ops 5
	// and 6 arrive with a gap at 1..4 → 6 exceeds the window).
	stale := newSecondaryWindow("n3", 4096, 1, 1024)
	stale.Handle(makeOp(5, 0, bytes.Repeat([]byte{5}, 32)))
	stale.Handle(makeOp(6, 0, bytes.Repeat([]byte{6}, 32)))
	if !stale.ResyncNeeded {
		t.Fatal("setup: expected stale state")
	}

	// Resync reset + replay ops 1..5 (from primary snapshot/send path).
	stale.Reset()
	for seq := uint64(1); seq <= 5; seq++ {
		r := stale.Handle(makeOp(seq, 0, bytes.Repeat([]byte{byte(seq)}, 32)))
		if !r.ACK {
			t.Fatalf("resync op %d: %+v", seq, r)
		}
	}
	if stale.LastSeq() != 5 || stale.ResyncNeeded {
		t.Errorf("post-resync: lastSeq=%d resyncNeeded=%v, want 5/false (rejoins as Secondary)",
			stale.LastSeq(), stale.ResyncNeeded)
	}
}
