package store

import (
	"context"

	"github.com/expanse/expanse/internal/errors"
)

// staleReadKey is the context key for opting into stale (local-FSM) reads.
type staleReadKey struct{}

// WithStale returns a context under which a Store serves reads straight
// from its local state with no linearizability round trip. This is an
// EXPLICIT opt-in for hot paths (metrics, UI polling) only. Scheduling and
// lease decisions must never use stale reads — internal/store's
// AssertNoStaleReads test helper enforces this by scanning for WithStale
// in sensitive packages.
func WithStale(ctx context.Context) context.Context {
	return context.WithValue(ctx, staleReadKey{}, true)
}

// StaleFrom reports whether ctx opted into stale reads.
func StaleFrom(ctx context.Context) bool {
	v, _ := ctx.Value(staleReadKey{}).(bool)
	return v
}

// Key is a slash-separated path identifying a value in the store.
//
// List/Watch prefixes are LITERAL byte prefixes: List("/a") returns "/ab"
// as well as "/a/...". Callers that want directory semantics must include
// the trailing slash: List("/a/").
type Key string

// Revision is a monotonically increasing counter assigned to every write.
// Revisions are strictly increasing across all keys and never reused.
type Revision uint64

// Entry is a single key-value pair with metadata.
type Entry struct {
	Key       Key
	Value     []byte
	Revision  Revision
	CreatedAt int64 // unix-nano
	UpdatedAt int64 // unix-nano
}

// Sentinel errors. Implementations must return errors matching these kinds
// (see internal/errors): KindNotFound for missing keys, KindConflict for
// failed CAS/Delete preconditions.
var (
	ErrNotFound = errors.New(errors.KindNotFound, "store", "key not found")
	ErrConflict = errors.New(errors.KindConflict, "store", "revision precondition failed")
)

// Store is the persistent backend interface. Phase 02 implements it over
// BoltDB (internal/store/boltstore); Phase 03 implements it over Raft.
// Both must pass the shared conformance suite (internal/store/conformance).
type Store interface {
	// Get returns the entry for k, or an error of kind KindNotFound.
	Get(ctx context.Context, k Key) (*Entry, error)
	// List returns all entries whose key literally starts with prefix,
	// sorted by key.
	List(ctx context.Context, prefix Key) ([]*Entry, error)
	// Put writes k→v and returns the new revision.
	Put(ctx context.Context, k Key, v []byte) (Revision, error)
	// CompareAndSwap writes only if the current revision of k matches
	// expect. expect==0 means "must not exist". A stale or otherwise
	// mismatched expect yields KindConflict and changes nothing.
	CompareAndSwap(ctx context.Context, k Key, expect Revision, v []byte) (Revision, error)
	// Delete removes k. A non-zero expect must match the current revision
	// (KindConflict otherwise); deleting a missing key yields KindNotFound.
	Delete(ctx context.Context, k Key, expect Revision) error
	// Txn applies ops atomically: if any op fails (e.g. an OpCheck with a
	// stale revision), nothing is applied. Returns the final revision.
	Txn(ctx context.Context, ops []Op) (Revision, error)
	// Watch fires events for keys literally under prefix, starting after
	// fromRev, in revision order. The channel is closed when ctx is
	// canceled, the store is closed, or the watcher's buffer overflows
	// (callers must then re-list and re-watch — events are never dropped
	// silently).
	Watch(ctx context.Context, prefix Key, fromRev Revision) (<-chan Event, error)
	// Revision returns the current monotonic revision counter.
	Revision(ctx context.Context) (Revision, error)
	// Close releases resources; safe to call once when done.
	Close() error
}

// OpKind identifies a transaction operation.
type OpKind int

const (
	OpPut OpKind = iota
	OpDelete
	OpCheck
)

// Op is one operation within a transaction.
type Op struct {
	Kind   OpKind
	Key    Key
	Value  []byte
	Expect Revision // for OpCheck / conditional delete
}

// EventType identifies what happened in a Watch event.
type EventType int

const (
	EventPut EventType = iota
	EventDelete
)

// Event is emitted by Watch when a key under the watched prefix changes.
type Event struct {
	Type     EventType
	Entry    *Entry // new entry for Put; the deleted entry for Delete
	Prev     *Entry // previous entry, nil if the key did not exist
	Revision Revision
}
