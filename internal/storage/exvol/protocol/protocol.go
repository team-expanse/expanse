// Package protocol is the in-memory simulator of the exvol replication
// protocol (§4.3), built before any real I/O (Phase 06 T04). It models
// exactly: sequence assignment (R2, monotonic gapless), quorum ack math
// (R1, floor(R/2)+1), out-of-order buffering with the bounded window
// (R3, 1024 ops / 64 MiB), CRC validation with NACK/retransmit, gap
// detection triggering resync, lease-loss EIO (R5), and the failover
// recovery algorithm including step 4b (pull ops from a replica with a
// higher seq than the new primary).
//
// There are no goroutines and no real network: tests drive message
// delivery order, drops, duplicates, and corruption directly, which is
// what makes the property tests deterministic. T05 wires these same
// message types to a real transport; the protocol semantics live here
// and do not change.
package protocol

import (
	"hash/crc32"
)

// castagnoli is the crc32c (Castagnoli) table — the §12 handover pins
// CRC to crc32c, not IEEE.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// CRC32C computes the wire CRC of an op's payload.
func CRC32C(b []byte) uint32 { return crc32.Checksum(b, castagnoli) }

// WriteOp is one replicated write (§4.3 step 3).
type WriteOp struct {
	Seq    uint64
	Offset uint64
	Data   []byte
	CRC    uint32 // crc32c(Data)

	// Flush is a durability marker (§4.4): fsync the local replica,
	// ack through the normal quorum machinery, write no data. It
	// consumes a sequence number so R2 stays gapless.
	Flush bool
}

// ValidCRC reports whether the op's CRC matches its payload.
func (op *WriteOp) ValidCRC() bool { return CRC32C(op.Data) == op.CRC }

// Reply is a secondary's response to a WriteOp.
type Reply struct {
	ACK bool
	// Seq is the acked sequence on ACK.
	Seq uint64
	// LastSeq is the replica's current lastSeq on NACK (gap info for
	// the primary/controller's resync decision).
	LastSeq uint64
	// Gap is set when the op arrived out of order and was buffered.
	Gap bool
	// Retransmit is set on CRC mismatch — resend the same op.
	Retransmit bool
	// Resync is set when the secondary's buffer window is exceeded —
	// the replica is too far behind for buffering; full resync required.
	Resync bool
	Reason string
}

// Window limits for out-of-order buffering (R3). Defaults match the
// spec: 1024 ops / 64 MiB.
const (
	DefaultWindowOps   = 1024
	DefaultWindowBytes = 64 << 20
)

// Secondary is one replica's protocol state (§4.3 step 6). Applies ops
// strictly in sequence order; out-of-order arrivals are buffered within
// the window.
type Secondary struct {
	ID   string
	Data []byte

	lastSeq   uint64
	pending   map[uint64]WriteOp
	pendOps   int
	pendBytes uint64

	windowOps   int
	windowBytes uint64

	// applyFn, when set, replaces the built-in Data copy with a
	// caller-supplied durable write (the real secondary writes to its
	// zvol via localwrite). Returning an error surfaces as a NACK.
	applyFn func(WriteOp) error

	// ResyncNeeded latches once the window is exceeded until Reset.
	ResyncNeeded bool
}

// NewSecondary creates a replica with a size-byte backing store and the
// spec-default buffering window.
func NewSecondary(id string, size int) *Secondary {
	return newSecondaryWindow(id, size, DefaultWindowOps, DefaultWindowBytes)
}

func newSecondaryWindow(id string, size, windowOps int, windowBytes uint64) *Secondary {
	return &Secondary{
		ID:          id,
		Data:        make([]byte, size),
		pending:     map[uint64]WriteOp{},
		windowOps:   windowOps,
		windowBytes: windowBytes,
	}
}

// NewSecondaryWithApply is NewSecondary with every applied write routed
// through apply (the real secondary's O_DIRECT|O_DSYNC zvol write). An
// apply error surfaces as a NACK — the op is NOT acknowledged durable.
func NewSecondaryWithApply(id string, size int, apply func(WriteOp) error) *Secondary {
	// No in-memory backing: with an apply hook every write goes
	// straight to durable storage, so Data would be a size-byte
	// allocation we never read — fatal on real (GiB) volumes.
	s := newSecondaryWindow(id, 0, DefaultWindowOps, DefaultWindowBytes)
	s.applyFn = apply
	return s
}

// LastSeq is the highest contiguously applied sequence (R2).
func (s *Secondary) LastSeq() uint64 { return s.lastSeq }

// AdoptSeq latches the sequence after a resync (§4.3 resync step 5):
// the replica's durable state now corresponds to a snapshot taken at
// seq, so replication resumes from there. Pending buffers are dropped
// — their contents came (or will come) with the image. Monotonic: a
// stale image can never rewind a replica past what it applied.
func (s *Secondary) AdoptSeq(seq uint64) {
	if seq < s.lastSeq {
		return
	}
	s.lastSeq = seq
	s.pending = map[uint64]WriteOp{}
	s.pendOps = 0
	s.pendBytes = 0
	s.ResyncNeeded = false
}

// Handle processes one WriteOp per §4.3 step 6:
// a. CRC mismatch → NACK, request retransmit (never applied);
// b. seq must be lastSeq+1 — later seqs are buffered (bounded window;
//
//	exceeding it → resync NACK); earlier or equal seqs are duplicates
//	(idempotent re-ACK — the network may duplicate);
//
// c/d. in-order ops apply to the local store, then lastSeq advances,
// draining any now-contiguous buffered ops;
// e. ACK with the applied seq.
func (s *Secondary) Handle(op WriteOp) Reply {
	if !op.ValidCRC() {
		return Reply{
			ACK: false, LastSeq: s.lastSeq, Retransmit: true,
			Reason: "crc mismatch",
		}
	}
	if s.ResyncNeeded {
		return Reply{
			ACK: false, LastSeq: s.lastSeq, Resync: true,
			Reason: "resync in progress (window previously exceeded)",
		}
	}

	if op.Seq == 0 || op.Seq <= s.lastSeq {
		// Duplicate delivery: already durable, re-ACK idempotently —
		// but REWRITE the data bytes first. The oplog may claim an op
		// whose zvol bytes were lost (torn zvol behind honest claims);
		// recovery's 4c leveling resends are what rebuild them, and
		// blindly re-ACKing without writing cements the hole. Rewriting
		// the same bytes is a no-op on a healthy replica.
		if len(op.Data) > 0 {
			if s.applyFn != nil {
				if err := s.applyFn(op); err != nil {
					return Reply{ACK: false, LastSeq: s.lastSeq, Reason: "re-apply failed: " + err.Error()}
				}
			} else {
				copy(s.Data[op.Offset:], op.Data)
			}
		}
		return Reply{ACK: true, Seq: s.lastSeq}
	}

	if op.Seq > s.lastSeq+1 {
		// Gap: buffer, but never exceed the window (R3). Exceeding the
		// window triggers resync — it is never a silent drop.
		if _, dup := s.pending[op.Seq]; dup {
			return Reply{ACK: false, LastSeq: s.lastSeq, Gap: true, Reason: "duplicate pending"}
		}
		if s.pendOps+1 > s.windowOps || s.pendBytes+uint64(len(op.Data)) > s.windowBytes {
			s.ResyncNeeded = true
			return Reply{
				ACK: false, LastSeq: s.lastSeq, Resync: true,
				Reason: "out-of-order window exceeded",
			}
		}
		s.pending[op.Seq] = op
		s.pendOps++
		s.pendBytes += uint64(len(op.Data))
		return Reply{ACK: false, LastSeq: s.lastSeq, Gap: true, Reason: "gap"}
	}

	// op.Seq == lastSeq+1: apply, then drain contiguous buffered ops.
	if err := s.apply(op); err != nil {
		return Reply{ACK: false, LastSeq: s.lastSeq, Reason: "local apply failed: " + err.Error()}
	}
	for next, ok := s.pending[s.lastSeq+1]; ok; next, ok = s.pending[s.lastSeq+1] {
		if err := s.apply(next); err != nil {
			return Reply{ACK: false, LastSeq: s.lastSeq, Reason: "local apply failed: " + err.Error()}
		}
	}
	return Reply{ACK: true, Seq: s.lastSeq}
}

// apply applies one in-order op and advances lastSeq. It does not retain
// op (or op.Data) anywhere: durability lives in the local write itself
// (applyFn, or s.Data for the in-memory simulator) plus the caller's own
// oplog (offset/length/CRC only, no payload) — an unbounded per-op
// history here previously retained every WriteOp's full payload forever
// with no reader anywhere, which OOM'd a secondary under sustained
// foreground write load (found via vol-resync-incremental.nix's 5 GiB
// baseline write).
func (s *Secondary) apply(op WriteOp) error {
	if s.applyFn != nil {
		if err := s.applyFn(op); err != nil {
			return err
		}
	} else {
		if uint64(len(s.Data)) < op.Offset+uint64(len(op.Data)) {
			panic("simulator misuse: write out of range")
		}
		copy(s.Data[op.Offset:], op.Data)
	}
	if _, wasPending := s.pending[op.Seq]; wasPending {
		delete(s.pending, op.Seq)
		s.pendOps--
		s.pendBytes -= uint64(len(op.Data))
	}
	s.lastSeq = op.Seq
	return nil
}

// Reset clears a latched resync state after the controller completes a
// resync (the resync itself replays ops through Handle).
func (s *Secondary) Reset() { s.ResyncNeeded = false }
