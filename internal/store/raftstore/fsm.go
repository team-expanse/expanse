package raftstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/raft"

	"github.com/expanse/expanse/internal/cluster/generation"
	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
)

// FSM is the deterministic key-value state machine replicated by Raft.
//
// DETERMINISM RULES — violating any of these causes silent divergence
// between nodes (the worst failure mode a replicated store can have):
//
//  1. Apply NEVER calls time.Now(). Timestamps come from the command, set
//     by the leader before proposing.
//  2. Apply NEVER uses map iteration order (state is looked up by key; the
//     canonical serializer sorts keys).
//  3. Apply NEVER uses randomness.
//  4. Apply NEVER performs I/O other than mutating in-memory state.
//  5. Apply NEVER returns early on error without consuming the entry
//     identically on all nodes (the same entry applied on any replica must
//     either always or never mutate state — error responses are computed
//     but do not affect the state transitions).
//
// TestFSMDeterminism (fsm_test.go) replays a fixed 10,000-entry log on
// three fresh FSMs and asserts byte-identical serialized state.
type FSM struct {
	mu       sync.RWMutex
	data     map[store.Key]*store.Entry
	revision store.Revision
	watchers *watchBroadcaster
	dedup    *dedupCache
}

// NewFSM creates an empty FSM.
func NewFSM() *FSM {
	return &FSM{
		data:     make(map[store.Key]*store.Entry),
		watchers: newWatchBroadcaster(),
		dedup:    newDedupCache(dedupCapacity),
	}
}

const dedupCapacity = 4096

// Apply is called on every committed log entry. It MUST be deterministic
// (see the rules in the FSM doc comment). The returned value is transported
// to the proposing node's raft.Apply future; followers ignore it.
func (f *FSM) Apply(l *raft.Log) interface{} {
	cmd, err := decodeCommand(l.Data)
	if err != nil {
		// A malformed or unsupported entry is a hard data-integrity error.
		// We return the error (the proposer surfaces it) and do not mutate
		// state; every node computes the identical no-op. Never panic: a
		// panicking FSM kills the whole daemon on every replica.
		return &applyResult{err: err}
	}

	// Idempotency: a retried RequestID replays the original result without
	// re-applying the mutation.
	if id := cmd.GetRequestId(); id != "" {
		if rev, ok, found := f.dedup.Get(id); found {
			if !ok {
				return &applyResult{err: f.replayedConflict(id)}
			}
			return &applyResult{rev: store.Revision(rev)}
		}
	}

	var (
		events []store.Event
		rev    store.Revision
		aerr   error
	)
	// The mutation section runs under f.mu: readers (get/list/StateHash)
	// hold the read side, and raft applies entries from its own goroutine.
	// Without this lock the maps race with concurrent reads.
	f.mu.Lock()
	switch cmd.GetType() {
	case CmdPut:
		events, rev, aerr = f.applyCAS(store.Key(cmd.GetKey()), store.Revision(cmd.GetExpect()), cmd.GetValue(), cmd.GetTimestampUnixNs())
	case CmdDelete:
		events, rev, aerr = f.applyDelete(store.Key(cmd.GetKey()), store.Revision(cmd.GetExpect()))
	case CmdTxn:
		ops, err := storeOps(cmd.GetOps())
		if err != nil {
			aerr = err
			break
		}
		events, rev, aerr = f.applyTxn(ops, cmd.GetTimestampUnixNs())
	}
	if aerr == nil {
		f.revision = rev
		// §4.7: every accepted desired-state mutation creates a new
		// generation atomically — the generation keys are written inside
		// this same Apply (same log entry), so all replicas see them or
		// none does. Fully deterministic: number and hash derive from
		// state, timestamps from the leader-provided command timestamp.
		if mutationTouchesDesired(events) {
			gevents, grev := f.createGenerationLocked(cmd.GetTimestampUnixNs())
			events = append(events, gevents...)
			f.revision = grev
			rev = grev
		}
	}
	f.mu.Unlock()

	if id := cmd.GetRequestId(); id != "" {
		f.dedup.Add(id, uint64(rev), aerr == nil)
	}

	if aerr != nil {
		return &applyResult{err: aerr}
	}
	f.watchers.broadcast(events)
	return &applyResult{rev: rev}
}

// applyResult is the value returned from FSM.Apply through the Raft future.
type applyResult struct {
	rev store.Revision
	err error
}

func (f *FSM) replayedConflict(id string) error {
	return errors.New(errors.KindConflict, "raftstore.fsm", fmt.Sprintf("retried request %s previously failed", id))
}

// current is the entry stored for k, or nil.
func (f *FSM) current(k store.Key) *store.Entry {
	return f.data[k]
}

// applyCAS writes k→v if the revision precondition holds. expect==0 means
// "must not exist". Events are returned unbuffered — the caller broadcasts
// after committing the revision.
func (f *FSM) applyCAS(k store.Key, expect store.Revision, v []byte, ts int64) ([]store.Event, store.Revision, error) {
	old := f.current(k)
	if expect == 0 {
		if old != nil {
			return nil, 0, conflict("Put", k, "key already exists (expect=0)")
		}
	} else {
		if old == nil {
			return nil, 0, conflict("Put", k, "key does not exist")
		}
		if old.Revision != expect {
			return nil, 0, conflict("Put", k, "stale revision")
		}
	}
	return []store.Event{f.putEntry(old, k, v, ts)}, f.revision + 1, nil
}

// applyDelete removes k if the precondition holds.
func (f *FSM) applyDelete(k store.Key, expect store.Revision) ([]store.Event, store.Revision, error) {
	old := f.current(k)
	if old == nil {
		return nil, 0, store.ErrNotFound
	}
	if expect != 0 && old.Revision != expect {
		return nil, 0, conflict("Delete", k, "stale revision")
	}
	delete(f.data, k)
	ev := store.Event{Type: store.EventDelete, Entry: old, Prev: old, Revision: f.revision + 1}
	return []store.Event{ev}, f.revision + 1, nil
}

// applyTxn applies ops atomically: on any failure nothing is applied.
func (f *FSM) applyTxn(ops []store.Op, ts int64) ([]store.Event, store.Revision, error) {
	// Validate every op against the current state first (all-or-nothing).
	// NOTE: OpPut inside a txn is unconditional (boltstore semantics);
	// preconditions are expressed with OpCheck.
	for _, op := range ops {
		switch op.Kind {
		case store.OpPut:
			// unconditional within a txn
		case store.OpDelete:
			e := f.current(op.Key)
			if e == nil {
				return nil, 0, store.ErrNotFound
			}
			if op.Expect != 0 && e.Revision != op.Expect {
				return nil, 0, conflict("Txn.Delete", op.Key, "stale revision")
			}
		case store.OpCheck:
			e := f.current(op.Key)
			if op.Expect == 0 {
				if e != nil {
					return nil, 0, conflict("Txn.Check", op.Key, "key exists (want absent)")
				}
				continue // Expect==0: key must not exist
			}
			if e == nil {
				return nil, 0, conflict("Txn.Check", op.Key, "key does not exist")
			}
			if e.Revision != op.Expect {
				return nil, 0, conflict("Txn.Check", op.Key, "stale revision")
			}
		default:
			return nil, 0, errors.New(errors.KindInvalid, "raftstore.fsm", fmt.Sprintf("unknown op kind %d", op.Kind))
		}
	}
	// All checks passed; apply in order.
	next := f.revision + 1
	events := make([]store.Event, 0, len(ops))
	for _, op := range ops {
		switch op.Kind {
		case store.OpPut:
			events = append(events, f.putEntry(f.current(op.Key), op.Key, op.Value, ts))
		case store.OpDelete:
			old := f.current(op.Key)
			delete(f.data, op.Key)
			events = append(events, store.Event{Type: store.EventDelete, Entry: old, Prev: old, Revision: next})
		case store.OpCheck:
			// read-only within the txn
		}
	}
	return events, next, nil
}

// putEntry stores the new entry and returns its event. Caller must have
// verified preconditions. prev is the replaced entry (may be nil).
func (f *FSM) putEntry(prev *store.Entry, k store.Key, v []byte, ts int64) store.Event {
	entry := &store.Entry{
		Key:       k,
		Value:     append([]byte(nil), v...), // defensive copy: FSM owns its state
		Revision:  f.revision + 1,
		CreatedAt: ts,
		UpdatedAt: ts,
	}
	if prev != nil {
		entry.CreatedAt = prev.CreatedAt
	}
	f.data[k] = entry
	return store.Event{Type: store.EventPut, Entry: entry, Prev: prev, Revision: entry.Revision}
}

// get returns the entry for k from local (possibly stale) state.
func (f *FSM) get(k store.Key) (*store.Entry, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	e := f.current(k)
	if e == nil {
		return nil, store.ErrNotFound
	}
	return e, nil
}

// list returns all entries under prefix from local state, sorted by key.
func (f *FSM) list(prefix store.Key) ([]*store.Entry, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	var keys []store.Key
	for k := range f.data {
		if bytes.HasPrefix([]byte(k), []byte(prefix)) {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	entries := make([]*store.Entry, 0, len(keys))
	for _, k := range keys {
		e := *f.data[k] // copy: callers must not race FSM mutation
		entries = append(entries, &e)
	}
	return entries, nil
}

// --- snapshot / restore ---

// Snapshot implements raft.FSM. The returned snapshot is a canonical,
// sorted-key serialization, byte-identical across nodes given identical
// state — this is what makes restore-based comparison and the determinism
// tests possible.
func (f *FSM) Snapshot() (raft.FSMSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &fsmSnapshot{data: f.canonicalLocked()}, nil
}

// canonicalLocked serializes the full state (revision + entries sorted by
// key) in a canonical byte format. Callers must hold mu (read or write).
//
// Format: revision(8) | count(8) | per entry: keyLen(4) key | rev(8)
// created(8) updated(8) valLen(4) val — all big-endian.
func (f *FSM) canonicalLocked() []byte {
	keys := make([]store.Key, 0, len(f.data))
	for k := range f.data {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	var buf bytes.Buffer
	be := func(v uint64) { binary.Write(&buf, binary.BigEndian, v) }
	be(uint64(f.revision))
	be(uint64(len(keys)))
	for _, k := range keys {
		e := f.data[k]
		be(uint64(len(k)))
		buf.WriteString(string(k))
		be(uint64(e.Revision))
		be(uint64(e.CreatedAt))
		be(uint64(e.UpdatedAt))
		be(uint64(len(e.Value)))
		buf.Write(e.Value)
	}
	return buf.Bytes()
}

// Restore implements raft.FSM: reset state to the snapshot's.
func (f *FSM) Restore(r io.ReadCloser) error {
	defer r.Close()
	all, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("fsm restore read: %w", err)
	}
	rev, entries, err := decodeCanonical(all)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data = entries
	f.revision = rev
	return nil
}

// StateHash returns sha256 of the canonical serialization — used by the
// determinism tests and (later) the chaos suite's divergence checker.
func (f *FSM) StateHash() [32]byte {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return sha256.Sum256(f.canonicalLocked())
}

// Revision returns the current FSM revision.
func (f *FSM) Revision() store.Revision {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.revision
}

func conflict(op string, k store.Key, msg string) error {
	return errors.New(errors.KindConflict, op, fmt.Sprintf("%s (key %s)", msg, k))
}

// mutationTouchesDesired reports whether any event in the batch mutates
// a desired-state key (§4.7). Status/observed keys never trigger a
// generation.
func mutationTouchesDesired(events []store.Event) bool {
	for _, ev := range events {
		if ev.Entry != nil && generation.IsDesiredKey(ev.Entry.Key) {
			return true
		}
		if ev.Prev != nil && generation.IsDesiredKey(ev.Prev.Key) {
			return true
		}
	}
	return false
}

// createGenerationLocked snapshots the desired state and writes
// /generations/<n>/{meta,data} plus the /cluster/generation pointer,
// applying retention (last 50 + last 30 days). Caller holds f.mu (write).
func (f *FSM) createGenerationLocked(ts int64) ([]store.Event, store.Revision) {
	cur := f.currentGenLocked()
	n := cur + 1

	snap := make(map[store.Key][]byte)
	for k, e := range f.data {
		if generation.IsDesiredKey(k) {
			snap[k] = e.Value
		}
	}
	meta, _ := json.Marshal(generation.Generation{ //nolint:errcheck — struct with plain fields
		Number:    n,
		Revision:  f.revision,
		CreatedAt: time.Unix(0, ts).UTC(),
		CreatedBy: "system",
		Parent:    cur,
		Hash:      generation.Hash(snap),
	})

	var events []store.Event
	events = append(events, f.putEntry(f.current(generation.DataKey(n)),
		generation.DataKey(n), generation.EncodeSnapshot(snap), ts))
	events = append(events, f.putEntry(f.current(generation.MetaKey(n)),
		generation.MetaKey(n), meta, ts))
	events = append(events, f.putEntry(f.current(generation.CurrentKey),
		generation.CurrentKey, []byte(strconv.FormatUint(n, 10)), ts))

	// Retention: keep the last KeepLast generations and everything within
	// KeepWindow of the entry timestamp; delete the rest. Old generation
	// deletes ride the same revision — deterministic on every replica.
	for _, old := range f.retentionVictimsLocked(n, ts) {
		for _, k := range []store.Key{generation.DataKey(old), generation.MetaKey(old)} {
			e := f.data[k]
			if e == nil {
				continue
			}
			delete(f.data, k)
			events = append(events, store.Event{Type: store.EventDelete, Entry: e, Prev: e, Revision: f.revision})
		}
	}
	return events, f.revision
}

// currentGenLocked parses /cluster/generation (0 when absent or corrupt —
// a corrupt pointer would make numbering restart, which is safe because
// new numbers are still monotonic from there).
func (f *FSM) currentGenLocked() uint64 {
	if e := f.data[generation.CurrentKey]; e != nil {
		if n, err := strconv.ParseUint(string(e.Value), 10, 64); err == nil {
			return n
		}
	}
	return 0
}

// retentionVictimsLocked returns the generation numbers to delete: all
// recorded generations except the newest KeepLast and those created
// within KeepWindow of ts.
func (f *FSM) retentionVictimsLocked(newest uint64, ts int64) []uint64 {
	type genMeta struct {
		num   uint64
		ageOk bool
	}
	var gens []genMeta
	cutoff := time.Unix(0, ts).Add(-generation.KeepWindow)
	for k, e := range f.data {
		if !strings.HasPrefix(string(k), generation.GensPrefix) || !strings.HasSuffix(string(k), "/meta") {
			continue
		}
		numStr := strings.TrimSuffix(strings.TrimPrefix(string(k), generation.GensPrefix), "/meta")
		num, err := strconv.ParseUint(numStr, 10, 64)
		if err != nil {
			continue
		}
		var g generation.Generation
		ageOk := json.Unmarshal(e.Value, &g) == nil && g.CreatedAt.After(cutoff)
		gens = append(gens, genMeta{num: num, ageOk: ageOk})
	}
	if len(gens) <= generation.KeepLast {
		return nil
	}
	sort.Slice(gens, func(i, j int) bool { return gens[i].num > gens[j].num }) // newest first
	for i := range gens {
		if i < generation.KeepLast {
			gens[i].ageOk = true // newest 50 always kept
		}
	}
	var victims []uint64
	for _, g := range gens {
		if !g.ageOk && g.num != newest {
			victims = append(victims, g.num)
		}
	}
	return victims
}

// fsmSnapshot streams the canonical serialization. Persist/Release are
// no-ops because the byte slice is already in memory.
type fsmSnapshot struct {
	data []byte
}

func (s *fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	if _, err := sink.Write(s.data); err != nil {
		_ = sink.Cancel()
		return fmt.Errorf("fsm snapshot persist: %w", err)
	}
	return sink.Close()
}

func (s *fsmSnapshot) Release() {}

// decodeCanonical parses the canonical serialization produced by
// canonicalLocked.
func decodeCanonical(b []byte) (store.Revision, map[store.Key]*store.Entry, error) {
	r := bytes.NewReader(b)
	var tmp [8]byte
	u := func() (uint64, error) {
		if _, err := io.ReadFull(r, tmp[:]); err != nil {
			return 0, fmt.Errorf("fsm restore: truncated: %w", err)
		}
		return binary.BigEndian.Uint64(tmp[:]), nil
	}
	rev, err := u()
	if err != nil {
		return 0, nil, err
	}
	count, err := u()
	if err != nil {
		return 0, nil, err
	}
	if count > 1<<24 {
		return 0, nil, fmt.Errorf("fsm restore: absurd entry count %d", count)
	}
	entries := make(map[store.Key]*store.Entry, count)
	for i := uint64(0); i < count; i++ {
		klen, err := u()
		if err != nil {
			return 0, nil, err
		}
		if klen > 1<<20 {
			return 0, nil, fmt.Errorf("fsm restore: absurd key length %d", klen)
		}
		kb := make([]byte, klen)
		if _, err := io.ReadFull(r, kb); err != nil {
			return 0, nil, fmt.Errorf("fsm restore key: %w", err)
		}
		entryRev, err := u()
		if err != nil {
			return 0, nil, err
		}
		created, err := u()
		if err != nil {
			return 0, nil, err
		}
		updated, err := u()
		if err != nil {
			return 0, nil, err
		}
		vlen, err := u()
		if err != nil {
			return 0, nil, err
		}
		if vlen > 1<<30 {
			return 0, nil, fmt.Errorf("fsm restore: absurd value length %d", vlen)
		}
		vb := make([]byte, vlen)
		if _, err := io.ReadFull(r, vb); err != nil {
			return 0, nil, fmt.Errorf("fsm restore value: %w", err)
		}
		e := &store.Entry{
			Key:       store.Key(kb),
			Revision:  store.Revision(entryRev),
			CreatedAt: int64(created),
			UpdatedAt: int64(updated),
			Value:     vb,
		}
		entries[e.Key] = e
	}
	if r.Len() != 0 {
		return 0, nil, fmt.Errorf("fsm restore: %d trailing bytes", r.Len())
	}
	return store.Revision(rev), entries, nil
}
