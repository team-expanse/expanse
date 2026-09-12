package raftstore

import (
	"container/list"
	"sync"
)

// dedupCache is a bounded LRU mapping RequestID → apply result, used by the
// FSM to make retried writes idempotent: if a client retries a committed
// write with the same RequestID (e.g. after a lost response), the second
// application is a no-op that returns the original result instead of
// applying the mutation twice.
//
// Capacity is fixed (4096): request IDs older than the last 4096 writes are
// forgotten, at which point a retry is indistinguishable from a new write —
// acceptable because the API layer re-checks preconditions (CAS) in that
// case, turning a duplicate into a conflict rather than corruption.
type dedupCache struct {
	mu    sync.Mutex
	cap   int
	ll    *list.List               // front = most recently used
	items map[string]*list.Element // requestID → element holding *dedupEntry
}

type dedupEntry struct {
	id  string
	rev uint64
	ok  bool // whether the original application succeeded
}

// newDedupCache builds a cache with the given capacity.
func newDedupCache(capacity int) *dedupCache {
	return &dedupCache{
		cap:   capacity,
		ll:    list.New(),
		items: make(map[string]*list.Element, capacity),
	}
}

// Get returns (revision, ok, true) if id was previously applied.
func (d *dedupCache) Get(id string) (rev uint64, ok bool, found bool) {
	if id == "" {
		return 0, false, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	el, hit := d.items[id]
	if !hit {
		return 0, false, false
	}
	d.ll.MoveToFront(el)
	e := el.Value.(*dedupEntry)
	return e.rev, e.ok, true
}

// Add records the result of applying id.
func (d *dedupCache) Add(id string, rev uint64, ok bool) {
	if id == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if el, hit := d.items[id]; hit {
		e := el.Value.(*dedupEntry)
		e.rev, e.ok = rev, ok
		d.ll.MoveToFront(el)
		return
	}
	el := d.ll.PushFront(&dedupEntry{id: id, rev: rev, ok: ok})
	d.items[id] = el
	for d.ll.Len() > d.cap {
		oldest := d.ll.Back()
		if oldest == nil {
			break
		}
		d.ll.Remove(oldest)
		delete(d.items, oldest.Value.(*dedupEntry).id)
	}
}

// Len reports the number of cached entries.
func (d *dedupCache) Len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ll.Len()
}
