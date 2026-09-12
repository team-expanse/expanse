// Package boltstore implements store.Store over BoltDB (go.etcd.io/bbolt)
// for the single-node Phase 02 agent. Phase 03's raftstore implements the
// same interface and passes the same conformance suite.
package boltstore

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
)

const (
	dataBucket  = "data"
	metaBucket  = "meta"
	revMetaKey  = "rev"
	watchBuffer = 1024 // events per watcher before the channel is closed
)

// Store is the BoltDB-backed store.Store implementation.
type Store struct {
	db  *bolt.DB
	mu  sync.Mutex // serializes write transactions
	rev atomic.Uint64

	wmu      sync.Mutex            // guards the watcher registry
	watchers map[*watcher]struct{} // registered watchers
}

type watcher struct {
	prefix []byte
	from   store.Revision
	ch     chan store.Event
	closed bool
}

// New opens (or creates) a BoltDB-backed store at path. Bolt fsyncs on every
// committed write transaction by default — do not disable that; crash
// consistency (G2.13) depends on it.
func New(path string) (*Store, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create store dir: %w", err)
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 10 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	s := &Store{db: db, watchers: map[*watcher]struct{}{}}
	if err := s.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) init() error {
	// Seed buckets and the revision counter in one transaction so a fresh
	// database always has a well-formed meta bucket (rev starts at 0).
	if err := s.db.Update(func(tx *bolt.Tx) error {
		data, err := tx.CreateBucketIfNotExists([]byte(dataBucket))
		if err != nil {
			return fmt.Errorf("create data bucket: %w", err)
		}
		_ = data
		meta, err := tx.CreateBucketIfNotExists([]byte(metaBucket))
		if err != nil {
			return fmt.Errorf("create meta bucket: %w", err)
		}
		if meta.Get([]byte(revMetaKey)) == nil {
			return meta.Put([]byte(revMetaKey), uint64bytes(0))
		}
		return nil
	}); err != nil {
		return err
	}
	var rev uint64
	if err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte(metaBucket)).Get([]byte(revMetaKey))
		if v != nil {
			rev = binary.BigEndian.Uint64(v)
		}
		return nil
	}); err != nil {
		return err
	}
	s.rev.Store(rev)
	return nil
}

// Get returns the entry for k.
func (s *Store) Get(ctx context.Context, k store.Key) (*store.Entry, error) {
	var e *store.Entry
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte(dataBucket)).Get([]byte(k))
		if v == nil {
			return store.ErrNotFound
		}
		e = decodeEntry(k, v)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return e, nil
}

// List returns all entries whose key literally starts with prefix, sorted
// by key (BoltDB iterates in byte order).
func (s *Store) List(ctx context.Context, prefix store.Key) ([]*store.Entry, error) {
	var entries []*store.Entry
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket([]byte(dataBucket)).Cursor()
		p := []byte(prefix)
		for k, v := c.Seek(p); k != nil && bytes.HasPrefix(k, p); k, v = c.Next() {
			entries = append(entries, decodeEntry(store.Key(k), v))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// Put writes k→v and returns the new revision.
func (s *Store) Put(ctx context.Context, k store.Key, v []byte) (store.Revision, error) {
	return s.write(ctx, func(tx *bolt.Tx, next store.Revision) ([]store.Event, error) {
		return applyPut(tx, k, v, next)
	})
}

// CompareAndSwap writes v only if k's current revision equals expect.
func (s *Store) CompareAndSwap(ctx context.Context, k store.Key, expect store.Revision, v []byte) (store.Revision, error) {
	return s.write(ctx, func(tx *bolt.Tx, next store.Revision) ([]store.Event, error) {
		data := tx.Bucket([]byte(dataBucket))
		old := data.Get([]byte(k))
		if expect == 0 {
			if old != nil {
				return nil, conflict("CAS", k, "key already exists (expect=0)")
			}
		} else {
			if old == nil {
				return nil, conflict("CAS", k, "key does not exist")
			}
			if decodeEntry(k, old).Revision != expect {
				return nil, conflict("CAS", k, "stale revision")
			}
		}
		return applyPut(tx, k, v, next)
	})
}

// Delete removes k, optionally checking its current revision.
func (s *Store) Delete(ctx context.Context, k store.Key, expect store.Revision) error {
	_, err := s.write(ctx, func(tx *bolt.Tx, next store.Revision) ([]store.Event, error) {
		data := tx.Bucket([]byte(dataBucket))
		old := data.Get([]byte(k))
		if old == nil {
			return nil, store.ErrNotFound
		}
		e := decodeEntry(k, old)
		if expect != 0 && e.Revision != expect {
			return nil, conflict("Delete", k, "stale revision")
		}
		if err := data.Delete([]byte(k)); err != nil {
			return nil, fmt.Errorf("delete: %w", err)
		}
		return []store.Event{{
			Type:     store.EventDelete,
			Entry:    e,
			Prev:     e,
			Revision: store.Revision(next),
		}}, nil
	})
	return err
}

// Txn applies ops atomically; all-or-nothing.
func (s *Store) Txn(ctx context.Context, ops []store.Op) (store.Revision, error) {
	return s.write(ctx, func(tx *bolt.Tx, next store.Revision) ([]store.Event, error) {
		var events []store.Event
		for _, op := range ops {
			data := tx.Bucket([]byte(dataBucket))
			switch op.Kind {
			case store.OpPut:
				evs, err := applyPut(tx, op.Key, op.Value, next)
				if err != nil {
					return nil, err
				}
				events = append(events, evs...)
			case store.OpDelete:
				old := data.Get([]byte(op.Key))
				if old == nil {
					return nil, store.ErrNotFound
				}
				e := decodeEntry(op.Key, old)
				if op.Expect != 0 && e.Revision != op.Expect {
					return nil, conflict("Txn.Delete", op.Key, "stale revision")
				}
				if err := data.Delete([]byte(op.Key)); err != nil {
					return nil, fmt.Errorf("txn delete: %w", err)
				}
				events = append(events, store.Event{
					Type:     store.EventDelete,
					Entry:    e,
					Prev:     e,
					Revision: next,
				})
			case store.OpCheck:
				old := data.Get([]byte(op.Key))
				if old == nil {
					return nil, conflict("Txn.Check", op.Key, "key does not exist")
				}
				if decodeEntry(op.Key, old).Revision != op.Expect {
					return nil, conflict("Txn.Check", op.Key, "stale revision")
				}
			default:
				return nil, fmt.Errorf("unknown op kind %d", op.Kind)
			}
		}
		return events, nil
	})
}

// write runs fn inside a single Bolt write transaction. fn receives the
// next revision to assign and returns the events to broadcast; events are
// broadcast only after the transaction commits successfully. The revision
// counter advances exactly once per transaction.
func (s *Store) write(ctx context.Context, fn func(tx *bolt.Tx, next store.Revision) ([]store.Event, error)) (store.Revision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var (
		events []store.Event
		rev    store.Revision
	)
	err := s.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte(metaBucket))
		cur := binary.BigEndian.Uint64(meta.Get([]byte(revMetaKey)))
		next := cur + 1
		rev = store.Revision(next)
		var err error
		events, err = fn(tx, rev)
		if err != nil {
			return err
		}
		return meta.Put([]byte(revMetaKey), uint64bytes(next))
	})
	if err != nil {
		return 0, err
	}
	s.rev.Store(uint64(rev))
	s.broadcast(events)
	return rev, nil
}

// applyPut writes the entry and returns the corresponding event.
func applyPut(tx *bolt.Tx, k store.Key, v []byte, rev store.Revision) ([]store.Event, error) {
	data := tx.Bucket([]byte(dataBucket))
	now := time.Now().UnixNano()
	entry := &store.Entry{
		Key:       k,
		Value:     v,
		Revision:  rev,
		CreatedAt: now,
		UpdatedAt: now,
	}
	var prev *store.Entry
	if old := data.Get([]byte(k)); old != nil {
		prev = decodeEntry(k, old)
		entry.CreatedAt = prev.CreatedAt
	}
	if err := data.Put([]byte(k), encodeEntry(entry)); err != nil {
		return nil, fmt.Errorf("put: %w", err)
	}
	return []store.Event{{
		Type:     store.EventPut,
		Entry:    entry,
		Prev:     prev,
		Revision: rev,
	}}, nil
}

// Watch registers a watcher under prefix. Events with revision <= fromRev
// are not delivered. The channel is closed on ctx cancel, store close, or
// buffer overflow (never silently dropping events).
func (s *Store) Watch(ctx context.Context, prefix store.Key, fromRev store.Revision) (<-chan store.Event, error) {
	w := &watcher{
		prefix: []byte(prefix),
		from:   fromRev,
		ch:     make(chan store.Event, watchBuffer),
	}
	s.wmu.Lock()
	s.watchers[w] = struct{}{}
	s.wmu.Unlock()
	go func() {
		<-ctx.Done()
		s.unregister(w)
	}()
	return w.ch, nil
}

func (s *Store) unregister(w *watcher) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if _, ok := s.watchers[w]; !ok {
		return // already unregistered
	}
	delete(s.watchers, w)
	if !w.closed {
		w.closed = true
		close(w.ch)
	}
}

// broadcast delivers events to matching watchers. Called with s.mu held
// (after commit) but uses a separate lock for the registry, so no
// re-entrancy issues. On overflow the watcher channel is closed and the
// watcher removed; callers must re-list and re-watch.
func (s *Store) broadcast(events []store.Event) {
	if len(events) == 0 {
		return
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	for w := range s.watchers {
		for _, ev := range events {
			var key []byte
			if ev.Entry != nil {
				key = []byte(ev.Entry.Key)
			}
			if !bytes.HasPrefix(key, w.prefix) || ev.Revision <= w.from {
				continue
			}
			if w.closed {
				break
			}
			select {
			case w.ch <- ev:
			default:
				// Overflow: close the channel rather than drop events.
				w.closed = true
				close(w.ch)
				delete(s.watchers, w)
			}
		}
	}
}

// Revision returns the current revision counter.
func (s *Store) Revision(ctx context.Context) (store.Revision, error) {
	return store.Revision(s.rev.Load()), nil
}

// Close closes the store and all watcher channels.
func (s *Store) Close() error {
	s.wmu.Lock()
	for w := range s.watchers {
		if !w.closed {
			w.closed = true
			close(w.ch)
		}
		delete(s.watchers, w)
	}
	s.wmu.Unlock()
	return s.db.Close()
}

func conflict(op string, k store.Key, msg string) error {
	return errors.New(errors.KindConflict, op, fmt.Sprintf("%s: %s (key %s)", op, msg, k))
}

func uint64bytes(v uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return b[:]
}

// encodeEntry encodes an entry: rev(8) | createdAt(8) | updatedAt(8) |
// valueLen(4) | value. The key is the Bolt bucket key and is not stored.
func encodeEntry(e *store.Entry) []byte {
	buf := make([]byte, 28+len(e.Value))
	binary.BigEndian.PutUint64(buf[0:8], uint64(e.Revision))
	binary.BigEndian.PutUint64(buf[8:16], uint64(e.CreatedAt))
	binary.BigEndian.PutUint64(buf[16:24], uint64(e.UpdatedAt))
	binary.BigEndian.PutUint32(buf[24:28], uint32(len(e.Value)))
	copy(buf[28:], e.Value)
	return buf
}

func decodeEntry(k store.Key, buf []byte) *store.Entry {
	if len(buf) < 28 {
		return &store.Entry{Key: k}
	}
	e := &store.Entry{
		Key:       k,
		Revision:  store.Revision(binary.BigEndian.Uint64(buf[0:8])),
		CreatedAt: int64(binary.BigEndian.Uint64(buf[8:16])),
		UpdatedAt: int64(binary.BigEndian.Uint64(buf[16:24])),
		Value:     make([]byte, binary.BigEndian.Uint32(buf[24:28])),
	}
	copy(e.Value, buf[28:])
	return e
}
