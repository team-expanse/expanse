package primary

import (
	"sync"

	experrors "github.com/expanse/expanse/internal/errors"
)

// commitBarrier keeps reads from observing writes that are not yet
// quorum-durable. The primary applies each write to its own zvol BEFORE
// fan-out, so its local bytes run ahead of the quorum: a read served from
// them could return a value that a failover later discards. Writes are
// serialized, so at most one range is in flight. If that write then
// fails, the local bytes are uncommitted for good, so the barrier fences
// the primary: no more reads or writes until it is re-established.
type commitBarrier struct {
	mu       sync.Mutex
	cond     *sync.Cond
	active   bool
	off, end uint64
	fenced   bool
}

func newCommitBarrier() *commitBarrier {
	b := &commitBarrier{}
	b.cond = sync.NewCond(&b.mu)
	return b
}

// begin marks [off, off+n) as applied-but-uncommitted (n == 0 is a no-op).
func (b *commitBarrier) begin(off uint64, n int) {
	if n == 0 {
		return
	}
	b.mu.Lock()
	b.active, b.off, b.end = true, off, off+uint64(n)
	b.mu.Unlock()
}

// finish clears the in-flight range; an uncommitted data write fences.
func (b *commitBarrier) finish(committed bool) {
	b.mu.Lock()
	if b.active && !committed {
		b.fenced = true
	}
	b.active = false
	b.cond.Broadcast()
	b.mu.Unlock()
}

// wait blocks while [off, off+n) overlaps the in-flight write.
func (b *commitBarrier) wait(off, n uint64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.active && off < b.end && b.off < off+n {
		b.cond.Wait()
	}
	if b.fenced {
		return fencedErr("read")
	}
	return nil
}

func (b *commitBarrier) isFenced() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.fenced
}

func fencedErr(op string) error {
	return experrors.New(experrors.KindInternal, "exvol.primary."+op,
		"EIO: primary fenced: a write reached no quorum")
}

// ReadBarrier blocks until no not-yet-durable write overlaps
// [off, off+n), and fails once the primary is fenced.
func (c *Coordinator) ReadBarrier(off, n int64) error {
	return c.barrier.wait(uint64(off), uint64(n))
}

// Fenced reports whether a write failed quorum after being applied
// locally; the runtime then demotes the primary so recovery re-decides.
func (c *Coordinator) Fenced() bool { return c.barrier.isFenced() }
