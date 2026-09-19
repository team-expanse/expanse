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
	"github.com/expanse/expanse/internal/storage/exvol/oplog"
	"github.com/expanse/expanse/internal/storage/exvol/protocol"
	"github.com/expanse/expanse/internal/storage/exvol/transport"
)

// Lease is the validity gate (step 1). *lease.Held satisfies it.
type Lease interface {
	Valid() bool
}

// LocalWriter is the primary's own durable replica (step 4, plus the
// flush marker's fsync). *localwrite.Writer satisfies it. ReadAt backs
// FetchOps — replaying a briefly-stale replica's missed ops straight
// from the primary's own copy, no ZFS snapshot involved.
type LocalWriter interface {
	WriteAt(p []byte, off int64) error
	Flush() error
	ReadAt(p []byte, off int64) (int, error)
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
	id     string
	snd    *transport.Sender
	sendC  chan protocol.WriteOp // cap 1; full = still busy with a previous op (slow)
	stale  bool
	stop   chan struct{} // closed to retire the pump (revive path)
	exited chan struct{} // closed by the pump when it returns
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
	oplog    *oplog.Store // durable seq->location journal (§4.3 4a); nil = memory-only
	barrier  *commitBarrier
}

// AttachOplog wires the durable seq->location journal (§4.3 4a): every
// local write this coordinator makes is also recorded here — the SAME
// journal (internal/storage/exvol/oplog) a secondary-role stint on this
// node reads and writes. Without it, a node's own writes made while it
// was primary are invisible to recovery once it later restarts or
// rejoins as a secondary: its reported history silently regresses even
// though its zvol data is current, and the hole is never re-leveled.
func (c *Coordinator) AttachOplog(st *oplog.Store) {
	c.mu.Lock()
	c.oplog = st
	c.mu.Unlock()
}

// New builds a coordinator. lease may be nil in tests (treated valid).
func New(volID string, replication int, local LocalWriter, replicas []Replica, l Lease, staleTimeout time.Duration) *Coordinator {
	return NewAt(volID, replication, local, replicas, l, staleTimeout, 0)
}

// NewAt is New with the sequence counter seeded — the failover-recovery
// path resumes at max(allSeqs) (§4.3 step 5); sequences are logical
// (R2) and must never restart from 0.
func NewAt(volID string, replication int, local LocalWriter, replicas []Replica, l Lease, staleTimeout time.Duration, startSeq uint64) *Coordinator {
	if staleTimeout <= 0 {
		staleTimeout = DefaultStaleTimeout
	}
	c := &Coordinator{
		volID:    volID,
		p:        protocol.NewPrimaryAt(volID, replication, startSeq),
		local:    local,
		lease:    l,
		timeout:  staleTimeout,
		results:  make(chan repResult, 4*len(replicas)+8),
		replicas: make(map[string]*replicaState, len(replicas)),
		barrier:  newCommitBarrier(),
	}
	for _, r := range replicas {
		st := &replicaState{
			id:     r.NodeID,
			snd:    r.Sender,
			sendC:  make(chan protocol.WriteOp, 1),
			stop:   make(chan struct{}),
			exited: make(chan struct{}),
		}
		c.replicas[r.NodeID] = st
		c.startReplica(st)
	}
	return c
}

// startReplica runs the per-replica pump: one op at a time, Submit →
// Recv in order (the Sender requires ordered Recv). Errors (conn dead)
// end the pump; the coordinator notices and marks Stale.
func (c *Coordinator) startReplica(st *replicaState) {
	ch, stop := st.sendC, st.stop
	go func() {
		defer close(st.exited)
		for {
			select {
			case <-stop:
				return
			case op, ok := <-ch:
				if !ok {
					return
				}
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
		}
	}()
}

// retirePump stops a replica's pump and waits for it to exit. Closing
// the sender unblocks a Recv/Submit parked on a dead or idle conn.
// Must hold c.mu.
func (c *Coordinator) retirePump(st *replicaState) {
	select {
	case <-st.exited: // already gone (conn error path)
	default:
		close(st.stop)
		_ = st.snd.Close()
		<-st.exited
	}
}

// AddReplica (re-)admits a replica with a fresh sender — the resync
// path (T12) uses it after rebuilding a Stale replica's copy. A
// replica that was never present is created; a Stale one is revived
// and re-included in quorum accounting. Idempotent per node.
func (c *Coordinator) AddReplica(nodeID string, snd *transport.Sender) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.addReplicaLocked(nodeID, snd)
}

// ReviveCaughtUp re-admits a Stale replica with NO sequence gap. A Stale
// replica is skipped by the live fan-out, so every op assigned after
// `sent` (the last seq the caller delivered) would otherwise never reach
// it — a trailing gap nothing notices on an idle volume. Under the write
// lock (no op can be assigned) it delivers (sent, LastSeq] through send,
// then starts the pump; on a delivery error the replica stays Stale.
func (c *Coordinator) ReviveCaughtUp(nodeID string, snd *transport.Sender, sent uint64, send func(protocol.WriteOp) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cur := c.p.LastAssigned(); cur > sent {
		ops, err := c.fetchOpsLocked(sent, cur)
		if err != nil {
			return err
		}
		for _, op := range ops {
			if err := send(op); err != nil {
				return err
			}
		}
	}
	c.addReplicaLocked(nodeID, snd)
	return nil
}

func (c *Coordinator) addReplicaLocked(nodeID string, snd *transport.Sender) {
	if st, ok := c.replicas[nodeID]; ok {
		if !st.stale {
			return // healthy already; leave the pump alone
		}
		// Revive: fresh sender, fresh pump. Retire the old pump first
		// (it may still be parked in Submit/Recv — never assume it
		// exited just because the replica went stale).
		c.retirePump(st)
		st.snd = snd
		st.stale = false
		st.stop = make(chan struct{})
		st.exited = make(chan struct{})
		st.sendC = make(chan protocol.WriteOp, 1)
		c.startReplica(st)
		return
	}
	st := &replicaState{
		id: nodeID, snd: snd,
		sendC:  make(chan protocol.WriteOp, 1),
		stop:   make(chan struct{}),
		exited: make(chan struct{}),
	}
	c.replicas[nodeID] = st
	c.startReplica(st)
}

// MarkStale excludes nodeID from quorum accounting and fan-out until
// it is resynced and re-admitted (§4.3 4d). Used when a replica is
// adopted whose durable copy may be missing history — e.g. a node that
// crashed, lost its zvol, and rejoined: its sender starts at seq 0,
// and acking quorum writes without the earlier ops would violate
// durability (the ack quorum must be replicas that HOLD the history).
func (c *Coordinator) MarkStale(nodeID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if st, ok := c.replicas[nodeID]; ok {
		c.markStale(st)
	}
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
func (c *Coordinator) replicate(makeOp func(uint64) protocol.WriteOp, applyLocal func() error, opName string) (err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.barrier.isFenced() {
		return fencedErr(opName)
	}

	// Step 1: lease gate. Immediate EIO, no queueing.
	if c.lease != nil && !c.lease.Valid() {
		return experrors.New(experrors.KindInternal, opName, "EIO: volume lease lost")
	}

	// Step 2: sequence number (monotonic, never reused).
	seq, err := c.p.NextSeq()
	if err != nil {
		return experrors.New(experrors.KindInternal, opName, "EIO: volume lease lost")
	}

	// Step 3+4: local durable write BEFORE fan-out. Reads must not see
	// it until quorum-durable (commitBarrier).
	op := makeOp(seq)
	c.barrier.begin(op.Offset, len(op.Data))
	defer func() { c.barrier.finish(err == nil) }()
	if err := applyLocal(); err != nil {
		return experrors.Wrap(err, experrors.KindInternal, opName, "local replica write failed")
	}
	if c.oplog != nil {
		// Recovery metadata (§4.3 4a), mirroring the secondary apply
		// hook exactly: seq -> location + CRC. A flush consumes a seq
		// but carries no bytes (CRC 0).
		c.oplog.Append(seq, oplog.Record{Offset: int64(op.Offset), Length: len(op.Data), CRC: op.CRC}, op.Flush)
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

// DrainResults processes any pending async pump failures without
// blocking (§9, the reconcile tick). replicate()'s own drainResults only
// runs at the top of the NEXT write — a replica whose pump already died
// (found via vol-resync-incremental.nix: a drift write's fan-out to a
// down replica failed, but nothing wrote again afterward to trigger the
// next drainResults) would otherwise sit "live" in quorum accounting,
// undetected as Stale, for as long as the volume stays idle.
func (c *Coordinator) DrainResults() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.drainResults()
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

// ReplicaIDs lists every replica in the fan-out (monitoring, rebuild
// convergence).
func (c *Coordinator) ReplicaIDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.replicas))
	for id := range c.replicas {
		out = append(out, id)
	}
	return out
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

// FetchOps re-reads ops (from, to] from the primary's own durable
// copy — the op-replay resync path (runtime.tryOpReplay): a replica
// that was live-current before a brief outage can be caught back up
// by resending exactly the ops it missed, with no ZFS snapshot
// lineage required. Mirrors secondary.Secondary.FetchOps exactly;
// flush markers come back as empty ops carrying the flag.
func (c *Coordinator) FetchOps(from, to uint64) ([]protocol.WriteOp, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fetchOpsLocked(from, to)
}

// fetchOpsLocked is FetchOps for callers already holding c.mu.
func (c *Coordinator) fetchOpsLocked(from, to uint64) ([]protocol.WriteOp, error) {
	local := c.local
	log := c.oplog
	if log == nil {
		return nil, experrors.New(experrors.KindUnavailable, "exvol.primary", "no durable oplog wired")
	}
	records := log.Snapshot()
	overwritten := supersededOps(records, from, c.p.LastAssigned())
	ops := make([]protocol.WriteOp, 0, to-from)
	for seq := from + 1; seq <= to; seq++ {
		rec, ok := records[seq]
		if !ok {
			return nil, experrors.New(experrors.KindInternal, "exvol.primary", "op not durable here")
		}
		op := protocol.WriteOp{Seq: seq, Offset: uint64(rec.Offset), CRC: rec.CRC}
		if rec.Length > 0 {
			buf := make([]byte, rec.Length)
			if _, err := local.ReadAt(buf, rec.Offset); err != nil {
				return nil, experrors.Wrap(err, experrors.KindInternal, "exvol.primary", "op re-read failed")
			}
			op.Data = buf
			if overwritten.Superseded(seq) {
				op.CRC = protocol.CRC32C(buf) // the claim covers bytes a later op replaced
			}
		} else {
			op.Flush = true
		}
		ops = append(ops, op)
	}
	return ops, nil
}

// supersededOps indexes the ops in (from, last] so a fetch can tell which
// had their range rewritten by a later one.
func supersededOps(records map[uint64]oplog.Record, from, last uint64) *oplog.Spans {
	spans := map[uint64]oplog.Span{}
	for seq := from + 1; seq <= last; seq++ {
		if rec, ok := records[seq]; ok {
			spans[seq] = oplog.Span{Offset: uint64(rec.Offset), Length: uint32(rec.Length)}
		}
	}
	return oplog.NewSpans(spans)
}

// Replication is the configured replication factor.
func (c *Coordinator) Replication() int { return c.p.Replication }
