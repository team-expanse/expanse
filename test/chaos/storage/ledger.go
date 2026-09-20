// Package chaosstorage holds the T22 scenario-style chaos tests for the
// exvol replicated volume: replica kills under load, resync under load,
// and a slow secondary. Unlike the linearizability suite these do not
// check a full history; they assert the durability guarantee (an acked
// write is never lost, a failed write returns a clear error) plus a
// bound on foreground latency and error rate.
package chaosstorage

import (
	"fmt"
	"sort"
	"sync"
)

// Ledger records, per block, the newest token a writer acked and the
// newest it attempted. Each block has a single writer issuing strictly
// increasing tokens, so the final on-disk token must lie in between.
type Ledger struct {
	mu        sync.Mutex
	acked     map[int]uint64
	attempted map[int]uint64
}

// Attempt notes that a write of tok to block was sent (it may land even
// if the client saw an error).
func (l *Ledger) Attempt(block int, tok uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.attempted == nil {
		l.attempted, l.acked = map[int]uint64{}, map[int]uint64{}
	}
	l.attempted[block] = tok
}

// Ack notes that the write of tok to block was acknowledged as durable.
func (l *Ledger) Ack(block int, tok uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.acked[block] = tok
}

// Check validates a token read back from block: below the newest acked
// write is loss, above the newest attempt is a value nobody sent.
func (l *Ledger) Check(block int, got uint64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if got < l.acked[block] {
		return fmt.Errorf("block %d holds token %d but token %d was acked: acked write lost", block, got, l.acked[block])
	}
	if got > l.attempted[block] {
		return fmt.Errorf("block %d holds token %d but only up to %d was ever sent", block, got, l.attempted[block])
	}
	return nil
}

// Blocks lists every block that was written to, ascending.
func (l *Ledger) Blocks() []int {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]int, 0, len(l.attempted))
	for b := range l.attempted {
		out = append(out, b)
	}
	sort.Ints(out)
	return out
}
