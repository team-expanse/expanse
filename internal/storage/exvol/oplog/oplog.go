// Package oplog is the durable per-node, per-volume op-location journal
// shared by the primary and secondary write paths (§4.3 4a): whichever
// role currently drives this node's local durable copy — live client
// writes as primary, or replicated applies as secondary — records
// "seq -> where written + CRC" here. Recovery (QuerySeq/FetchOps) and
// self-recovery (the local-oplog resume check) both read it as the
// truthful answer to "what does this node's zvol actually hold,"
// regardless of which role produced each write.
//
// Without a durable journal, or one only half the roles write to, a
// daemon restart (or a primary<->secondary role change) forgets which
// sequences the zvol holds: recovery misjudges the replica as behind
// or empty, and a node's own prior-primary writes silently vanish from
// its reported history — the replica is never re-leveled and the hole
// persists forever (§4.3 4a metadata must survive crashes and role
// changes, not just the data).
package oplog

import (
	"fmt"
	"os"
	"strings"
	"sync"

	experrors "github.com/expanse/expanse/internal/errors"
)

// Record locates one durable op in the local zvol: its byte offset and
// length, plus the CRC it was applied under. Length == 0 marks a flush
// marker (no data, just a durability barrier).
type Record struct {
	Offset int64
	Length int
	CRC    uint32
}

// Store is an append-only seq->Record journal with an in-memory mirror,
// optionally backed by a durable file. A Store with no file is
// memory-only: it satisfies callers within one process lifetime but a
// restart forgets everything (the caller is expected to warn loudly).
type Store struct {
	mu   sync.Mutex
	file *os.File
	recs map[uint64]Record
}

// NewMemory returns a Store with no durable backing.
func NewMemory() *Store { return &Store{recs: map[uint64]Record{}} }

// Open loads any existing records at path and opens it for append,
// creating it if it doesn't exist.
func Open(path string) (*Store, error) {
	s := NewMemory()
	data, err := os.ReadFile(path)
	if err == nil {
		for _, ln := range strings.Split(string(data), "\n") {
			if ln == "" {
				continue
			}
			var rec Record
			var seq uint64
			if _, serr := fmt.Sscanf(ln, "%d %d %d %d", &seq, &rec.Offset, &rec.Length, &rec.CRC); serr == nil {
				s.recs[seq] = rec
			}
		}
	} else if !os.IsNotExist(err) {
		return nil, experrors.Wrap(err, experrors.KindInternal, "exvol.oplog", "read oplog store")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, experrors.Wrap(err, experrors.KindInternal, "exvol.oplog", "open oplog store")
	}
	s.file = f
	return s, nil
}

// MaxSeq returns the highest seq currently recorded (0 if empty) — the
// resume point a caller adopts after loading a prior role's history.
func (s *Store) MaxSeq() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var max uint64
	for seq := range s.recs {
		if seq > max {
			max = seq
		}
	}
	return max
}

// Append durably records one op: in memory always, and on disk if this
// Store is file-backed. sync forces an fsync — callers pass true on
// flush markers, the client-visible durability barrier this metadata
// must be durable alongside.
func (s *Store) Append(seq uint64, rec Record, sync bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs[seq] = rec
	if s.file != nil {
		fmt.Fprintf(s.file, "%d %d %d %d\n", seq, rec.Offset, rec.Length, rec.CRC)
		if sync {
			_ = s.file.Sync()
		}
	}
}

// Get looks up one record.
func (s *Store) Get(seq uint64) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.recs[seq]
	return rec, ok
}

// Snapshot returns a copy of every record currently known.
func (s *Store) Snapshot() map[uint64]Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[uint64]Record, len(s.recs))
	for k, v := range s.recs {
		out[k] = v
	}
	return out
}

// Close releases the durable file handle, if any. Safe to call on a
// memory-only Store (no-op) or more than once.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}

// Reset discards all records, truncating the durable file if any (§4.3
// resync: a full receive invalidates per-seq history the new image no
// longer traces to).
func (s *Store) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs = map[uint64]Record{}
	if s.file != nil {
		s.file.Truncate(0) //nolint:errcheck — a lost truncation is repaired by resync
	}
}
