// Package primary is the exvol primary write coordinator (Phase 06 T07):
// it wires the T04 protocol primary state to the T05 mesh transport and
// the T06 local writer, implementing §4.3 write-path steps 1-8 from the
// primary's perspective.
//
// Per write:
//  1. lease gate — invalid lease → immediate EIO, nothing is queued or
//     written (R5: a primary without a lease must not accept writes);
//  2. sequence number from the T04 primary (monotonic, no gaps);
//  3. local zvol write completes BEFORE fan-out (step 4);
//  4. fan-out to all live secondaries (step 5) through their per-replica
//     Sender goroutines;
//  5. the client is acked the moment quorum — including the local
//     replica — is durable (step 7, R1: no knobs).
//
// §9 "slow secondary stalls the primary": a replica whose reply does
// not arrive within StaleTimeout is marked Stale — closed, excluded
// from quorum accounting and future fan-out — rather than blocking
// every subsequent write on the slowest node. Stale replicas are
// reintroduced only by failover/resync (T11/T12, out of scope here).
package primary

import (
	"fmt"
	"sync"
	"time"

	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/storage/exvol/protocol"
	"github.com/expanse/expanse/internal/storage/exvol/transport"
)

// Lease is the validity gate (step 1). *lease.Held satisfies it.
type Lease interface {
	Valid() bool
}

// LocalWriter is the primary's own durable replica (step 4, plus the
// flush marker's fsync). *localwrite.Writer satisfies it.
type LocalWriter interface {
	WriteAt(p []byte, off int64) error
	Flush() error
}

// DefaultStaleTimeout is the per-replica reply timeout (§9): after this,
// the replica is marked Stale instead of stalling quorum.
const DefaultStaleTimeout = 5 * time.Second

// Replica is one secondary connection.
type Replica struct {
	NodeID string
	Sender *transport.Sender
}

type repResult struct {
	nodeID string
	seq    uint64
	ack    bool
	resync bool // secondary cannot apply (gap/window) → needs resync
	err    error
}

type replicaState struct {
	id    string
	snd   *transport.Sender
	sendC chan protocol.WriteOp // cap 1; full = still busy with a previous op (slow)
	stale bool
}

// Coordinator serializes writes for one volume on one primary.
type Coordinator struct {
	volID    string
	p        *protocol.Primary
	local    LocalWriter
	lease    Lease
	timeout  time.Duration
	results  chan repResult
	mu       sync.Mutex
	replicas map[string]*replicaState
}

// New builds a coordinator. lease may be nil in tests (treated valid).
func New(volID string, replication int, local LocalWriter, replicas []Replica, l Lease, staleTimeout time.Duration) *Coordinator {
	if staleTimeout <= 0 {
		staleTimeout = DefaultStaleTimeout
	}
	c := &Coordinator{
		volID:    volID,
		p:        protocol.NewPrimary(volID, replication),
		local:    local,
		lease:    l,
		timeout:  staleTimeout,
		results:  make(chan repResult, 4*len(replicas)+8),
		replicas: make(map[string]*replicaState, len(replicas)),
	}
	for _, r := range replicas {
		st := &replicaState{id: r.NodeID, snd: r.Sender, sendC: make(chan protocol.WriteOp, 1)}
		c.replicas[r.NodeID] = st
		c.startReplica(st)
	}
	return c
}

// startReplica runs the per-replica pump: one op at a time, Submit →
// Recv in order (the Sender requires ordered Recv). Errors (conn dead)
// end the pump; the coordinator notices and marks Stale.
func (c *Coordinator) startReplica(st *replicaState) {
	go func() {
		defer close(st.sendC)
		for op := range st.sendC {
			if err := st.snd.Submit(c.volID, op); err != nil {
				c.results <- repResult{nodeID: st.id, err: fmt.Errorf("submit: %w", err)}
				return
			}
			rep, err := st.snd.Recv()
			if err != nil {
				c.results <- repResult{nodeID: st.id, err: fmt.Errorf("recv: %w", err)}
				return
			}
			c.results <- repResult{nodeID: st.id, seq: rep.Seq, ack: rep.ACK, resync: rep.Resync}
		}
	}()
}

// markStale excludes a replica from quorum accounting and fan-out (§9)
// and closes its connection so any blocked pump unblocks.
func (c *Coordinator) markStale(st *replicaState) {
	if st.stale {
		return
	}
	st.stale = true
	st.snd.Close()
}

// Write applies one client write (§4.3 steps 1-8): local-durable, then
// fan-out, then ack on quorum. Serializing writes under one mutex is
// deliberate — sequence assignment and per-replica ordered send both
// demand it, and replication throughput is dominated by the secondaries
// applying in parallel anyway.
func (c *Coordinator) Write(data []byte, off int64) error {
	return c.replicate(func(seq uint64) protocol.WriteOp {
		return protocol.WriteOp{Seq: seq, Offset: uint64(off), Data: data, CRC: protocol.CRC32C(data)}
	}, func() error { return c.local.WriteAt(data, off) }, "exvol.write")
}

// Flush is a durability marker (§4.4): fsync the local replica, then
// fan-out a flush op — a flush is not complete until quorum acks it
// (same quorum rule as a write; filesystems depend on this for
// journaling correctness). Consumes a sequence number so R2 stays
// gapless; secondaries fsync instead of writing data.
func (c *Coordinator) Flush() error {
	return c.replicate(func(seq uint64) protocol.WriteOp {
		return protocol.WriteOp{Seq: seq, Flush: true}
	}, c.local.Flush, "exvol.flush")
}

// replicate runs one op through §4.3 steps 1-8: lease gate, sequence,
// local durable apply, fan-out, quorum ack. makeOp builds the wire op
// from the assigned sequence; applyLocal is the primary's own durable
// step (completed BEFORE fan-out); opName labels errors.
func (c *Coordinator) replicate(makeOp func(uint64) protocol.WriteOp, applyLocal func() error, opName string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Step 1: lease gate. Immediate EIO, no queueing.
	if c.lease != nil && !c.lease.Valid() {
		return experrors.New(experrors.KindInternal, opName, "EIO: volume lease lost")
	}

	// Step 2: sequence number (monotonic, never reused).
	seq, err := c.p.NextSeq()
	if err != nil {
		return experrors.New(experrors.KindInternal, opName, "EIO: volume lease lost")
	}

	// Step 3+4: local durable write BEFORE fan-out.
	op := makeOp(seq)
	if err := applyLocal(); err != nil {
		return experrors.Wrap(err, experrors.KindInternal, opName, "local replica write failed")
	}

	// Step 5: fan-out to live secondaries. First drain pumps' pending
	// results (a previous write may have returned early on quorum, or
	// a pump may have died since).
	c.drainResults()
	live := make([]*replicaState, 0, len(c.replicas))
	for _, st := range c.replicas {
		if !st.stale {
			live = append(live, st)
		}
	}

	for _, st := range live {
		// Submission backpressures on the pump, bounded by the stale
		// timeout: a pump still busy with a previous op gets exactly
		// one deadline to catch up. Skipping the op instead would
		// open a permanent sequence gap (R2) — never an option. A
		// pump stuck past the deadline is marked Stale (§9).
		timer := time.NewTimer(c.timeout)
		select {
		case st.sendC <- op:
			timer.Stop()
		case <-timer.C:
			c.markStale(st)
		}
	}

	// Steps 6-8: collect replies until quorum (local + acks), marking
	// slow replicas Stale at the deadline instead of blocking.
	durable := 1
	remaining := make(map[string]*replicaState, len(live))
	for _, st := range live {
		remaining[st.id] = st
	}
	deadline := time.NewTimer(c.timeout)
	defer deadline.Stop()

	for len(remaining) > 0 {
		if c.p.ShouldAck(durable) {
			// Quorum reached — return now (step 8); slow replicas'
			// late results are drained by the next write.
			return nil
		}
		select {
		case res := <-c.results:
			st := c.replicas[res.nodeID]
			if res.err != nil {
				delete(remaining, st.id)
				c.markStale(st)
				continue
			}
			if _, ok := remaining[res.nodeID]; !ok {
				continue // late reply for a previous write — consumed
			}
			delete(remaining, st.id)
			if res.ack {
				durable++
			} else if res.resync {
				c.markStale(st) // cannot apply without resync
			}
			c.p.RecordAcked(seq)
		case <-deadline.C:
			for _, st := range remaining {
				c.markStale(st) // §9: slow, not failed — closed + excluded
			}
			remaining = nil
		}
	}
	if c.p.ShouldAck(durable) {
		return nil
	}
	return experrors.New(experrors.KindUnavailable, opName,
		fmt.Sprintf("%s not quorum-durable: %d of %d replicas (quorum %d)", opName, durable, c.p.Replication, c.p.Quorum()))
}

// drainResults consumes ALL pending pump output without blocking
// (results from a previous early-returned write, or dead pumps),
// updating staleness. Called only before fan-out, never during reply
// collection.
func (c *Coordinator) drainResults() {
	for {
		select {
		case res := <-c.results:
			st := c.replicas[res.nodeID]
			if res.err != nil || res.resync {
				c.markStale(st)
			}
		default:
			return
		}
	}
}

// StaleReplicas lists replicas marked Stale (monitoring, tests).
func (c *Coordinator) StaleReplicas() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, st := range c.replicas {
		if st.stale {
			out = append(out, st.id)
		}
	}
	return out
}

// LastSeq is the last assigned sequence number.
func (c *Coordinator) LastSeq() uint64 { return c.p.LastAssigned() }

// Replication is the configured replication factor.
func (c *Coordinator) Replication() int { return c.p.Replication }
