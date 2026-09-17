package protocol

import (
	"errors"
	"fmt"
	"sync"
)

// ErrLeaseLost is the EIO cause the primary surfaces after losing its
// lease (R5): fail in-flight and subsequent writes, never hang.
var ErrLeaseLost = errors.New("storage primary lease lost")

// Primary is the write-side protocol state (§4.3 steps 1–8): lease
// gating, sequence assignment (R2), and quorum ack math (R1). The
// simulator's primary keeps its own durable copy via a Secondary-like
// local store — tests apply the primary's local write before counting
// durability.
type Primary struct {
	VolID string
	// HasLease models lease.Guard.Valid(); set false to simulate lease
	// loss (R5).
	HasLease bool

	// Replication is the volume's replication factor R (1..5).
	Replication int

	mu        sync.Mutex // guards seq/lastAcked: assigned on the client's goroutine, read by the reconcile tick
	seq       uint64     // last assigned (monotonic, gapless — R2)
	lastAcked uint64
}

// NewPrimary creates a primary holding its lease at sequence 0.
func NewPrimary(volID string, replication int) *Primary {
	return &Primary{VolID: volID, HasLease: true, Replication: replication}
}

// NewPrimaryAt is NewPrimary with the sequence counter seeded — failover
// recovery resumes at max(allSeqs), never back at 0 (§4.3 step 5;
// sequences are logical and must not be reused, R2).
func NewPrimaryAt(volID string, replication int, startSeq uint64) *Primary {
	return &Primary{VolID: volID, HasLease: true, Replication: replication, seq: startSeq}
}

// NextSeq assigns the next sequence number (§4.3 step 2). After lease
// loss it fails with ErrLeaseLost — every subsequent write fails EIO
// within the guard band (R5); the simulator models that by refusing
// assignment.
func (p *Primary) NextSeq() (uint64, error) {
	if !p.HasLease {
		return 0, ErrLeaseLost
	}
	p.mu.Lock()
	p.seq++
	seq := p.seq
	p.mu.Unlock()
	return seq, nil
}

// LastAssigned is the highest assigned sequence (for diagnostics and
// recovery input).
func (p *Primary) LastAssigned() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.seq
}

// LastAcked is the last seq the primary counted at quorum (step 8).
func (p *Primary) LastAcked() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastAcked
}

// Quorum is floor(R/2)+1 (§4.3 step 7, R1). R=3 needs 2, R=2 needs 2
// (both replicas — R6: durability without availability), R=1 needs 1.
func (p *Primary) Quorum() int {
	if p.Replication <= 0 || p.Replication > 5 {
		panic(fmt.Sprintf("simulator misuse: replication %d outside 1..5", p.Replication))
	}
	return p.Replication/2 + 1
}

// ShouldAck is the R1 decision point (step 7): ack the client only when
// durableCount (the primary's local copy + acked secondaries) reaches
// quorum. No optimistic ack, no exceptions, no knob.
func (p *Primary) ShouldAck(durableCount int) bool {
	return durableCount >= p.Quorum()
}

// RecordAcked advances lastAcked once the client was acked (step 8).
func (p *Primary) RecordAcked(seq uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if seq > p.lastAcked {
		p.lastAcked = seq
	}
}
