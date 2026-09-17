// Package secondary is the exvol secondary write path (Phase 06 T08):
// T04's simulated secondary logic wired to the T05 transport and the
// T06 local writer, implementing §4.3 step 6 exactly:
//
//	a. CRC validation → NACK with retransmit request on mismatch
//	   (never applied, never acked durable);
//	b. sequence-gap detection (S != lastSeq+1) → NACK carrying this
//	   secondary's lastSeq, which triggers the resync path (T11) —
//	   never a silent drop, never a block (R2); out-of-order arrivals
//	   inside the R3 window are buffered;
//	c. local zvol write with O_DIRECT|O_DSYNC via localwrite — the op
//	   is applied in sequence order, durable before the ACK;
//	d. lastSeq = S;
//	e. ACK with S.
//
// Resync triggers are observable: every gap/window NACK emits a
// ResyncEvent on Events() — the resync itself lands in T11/T12.
package secondary

import (
	"sync"

	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/storage/exvol/protocol"
)

// LocalWriter is the secondary's durable local replica (step 6c, plus
// the flush marker's fsync). *localwrite.Writer satisfies it.
type LocalWriter interface {
	WriteAt(p []byte, off int64) error
	Flush() error
}

// ResyncEvent reports one resync trigger (step 6b / R3): the secondary
// is behind (Gap) or cannot buffer (window exceeded, ResyncNeeded).
type ResyncEvent struct {
	LastSeq uint64 // this secondary's last contiguously applied seq
	OpSeq   uint64 // the op that triggered the event (0 if none)
	Reason  string
}

// Secondary applies replicated writes in order to its local zvol.
type Secondary struct {
	mu     sync.Mutex
	nodeID string
	size   int
	proto  *protocol.Secondary // owns lastSeq/pending/CRC state (T04)
	events chan ResyncEvent
}

// New builds a secondary of the given volume size writing through w.
func New(nodeID string, size int, w LocalWriter) *Secondary {
	s := &Secondary{
		nodeID: nodeID,
		size:   size,
		events: make(chan ResyncEvent, 64),
	}
	s.proto = protocol.NewSecondaryWithApply(nodeID, size, func(op protocol.WriteOp) error {
		var err error
		if op.Flush {
			// Durability marker (§4.4): fsync, write nothing.
			err = w.Flush()
		} else {
			err = w.WriteAt(op.Data, int64(op.Offset))
		}
		if err != nil {
			return experrors.Wrap(err, experrors.KindInternal, "exvol.secondary", "local replica write failed")
		}
		return nil
	})
	return s
}

// Handle processes one replicated write (step 6). A gap or window
// overflow emits a ResyncEvent in addition to the NACK.
func (s *Secondary) Handle(op protocol.WriteOp) protocol.Reply {
	s.mu.Lock()
	rep := s.proto.Handle(op)
	s.mu.Unlock()
	if rep.Resync {
		s.emit(ResyncEvent{LastSeq: rep.LastSeq, OpSeq: op.Seq, Reason: rep.Reason})
	} else if rep.Gap {
		s.emit(ResyncEvent{LastSeq: rep.LastSeq, OpSeq: op.Seq, Reason: "gap"})
	}
	return rep
}

// LastSeq is the highest contiguously applied sequence (R2).
func (s *Secondary) LastSeq() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.proto.LastSeq()
}

// Reset clears a latched resync state after the controller (T11/T12)
// has resynced the volume; replayed ops then flow through Handle again.
func (s *Secondary) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.proto.Reset()
}

// Events exposes resync triggers (non-blocking for the sender; the
// channel is buffered and never blocks — an unconsumed channel simply
// drops events, the NACK itself carries the same information).
func (s *Secondary) Events() <-chan ResyncEvent { return s.events }

func (s *Secondary) emit(e ResyncEvent) {
	select {
	case s.events <- e:
	default:
	}
}

// Handler adapts the secondary to the transport's volume router.
func (s *Secondary) Handler() func(op protocol.WriteOp) protocol.Reply {
	return s.Handle
}
