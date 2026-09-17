package transport

import (
	"sync"

	"github.com/expanse/expanse/internal/storage/exvol/protocol"
)

// Sender pipelines WriteOps over one Conn with the R3 bounded in-flight
// window: at most windowOps un-acked ops totalling at most windowBytes
// of payload. Submit BLOCKS while the window is full — backpressure by
// waiting, never by dropping. Replies must be consumed with Recv in
// send order (freeing window slots); the typical caller runs one Sender
// per secondary off the primary's parallel write path.
type Sender struct {
	c           *Conn
	windowOps   int
	windowBytes uint64

	mu            sync.Mutex
	cond          *sync.Cond
	inFlightOps   int
	inFlightBytes uint64
	pendingSizes  []uint64 // FIFO: bytes of each in-flight op, in send order
}

// NewSender wraps a Conn with a bounded in-flight window. Non-positive
// limits default to the R3 limits (1024 ops / 64 MiB).
func NewSender(c *Conn, windowOps int, windowBytes uint64) *Sender {
	if windowOps <= 0 {
		windowOps = 1024
	}
	if windowBytes == 0 {
		windowBytes = 64 << 20
	}
	s := &Sender{c: c, windowOps: windowOps, windowBytes: windowBytes}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// InFlight is the current number of un-acked ops (diagnostics/tests).
func (s *Sender) InFlight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inFlightOps
}

// Submit sends op once a window slot is free, blocking if the window is
// full. The op's reply MUST eventually be consumed via Recv.
func (s *Sender) Submit(volID string, op protocol.WriteOp) error {
	s.mu.Lock()
	for s.inFlightOps >= s.windowOps || s.inFlightBytes+uint64(len(op.Data)) > s.windowBytes {
		s.cond.Wait()
	}
	s.inFlightOps++
	s.inFlightBytes += uint64(len(op.Data))
	s.pendingSizes = append(s.pendingSizes, uint64(len(op.Data)))
	s.mu.Unlock()

	if err := s.c.Send(volID, op); err != nil {
		s.release(uint64(len(op.Data)))
		return err
	}
	return nil
}

// Recv reads the next reply in send order and releases its window slot.
func (s *Sender) Recv() (protocol.Reply, error) {
	r, err := s.c.Recv()
	if err != nil {
		return protocol.Reply{}, err
	}
	s.release(0) // 0 = take the byte count from the FIFO
	return r, nil
}

// release frees one op slot; n > 0 subtracts that byte count directly
// (failed submit), n == 0 pops the FIFO head (acked op).
func (s *Sender) release(n uint64) {
	s.mu.Lock()
	s.inFlightOps--
	if n > 0 {
		s.inFlightBytes -= n
	} else if len(s.pendingSizes) > 0 {
		s.inFlightBytes -= s.pendingSizes[0]
		s.pendingSizes = s.pendingSizes[1:]
	}
	if s.inFlightOps < 0 {
		s.inFlightOps = 0
	}
	s.cond.Signal()
	s.mu.Unlock()
}

// Close closes the underlying connection (unblocks a blocked Recv).
func (s *Sender) Close() error { return s.c.Close() }
