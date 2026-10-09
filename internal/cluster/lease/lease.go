// Package lease implements singleton leases — the anti-split-brain
// mechanism (spec §4.3). A lease is a store key `/leases/<name>` whose
// value names the holder and an expiry; possession is enforced with
// compare-and-swap through any store.Store (Raft-replicated in
// production, boltstore in-process).
//
// # The safety argument (spec §4.3, verbatim)
//
// > A lease granted at time T expires at T+TTL by the *leader's* clock.
// > The holder renews at T+TTL/3. If the holder is partitioned, its
// > renewal fails, and it closes `Done()` at latest by T+TTL/3+renewal_timeout.
// > The leader will not grant the lease to another node until T+TTL.
// > Since TTL/3 + renewal_timeout (5s+2s=7s) < TTL (15s), there is a
// > ≥8 s guard band during which the old holder has stopped and the new
// > holder has not started. This holds as long as clock *rates* differ
// > by less than ~50%, which NTP-synced machines always satisfy. It does
// > not depend on clock *offset* agreement.
//
// Clock discipline (spec §10 "Clock jumps"): ALL local timing — renewal
// cadence, slow-renewal detection, the Done() stop deadline — uses
// monotonic time (time.Ticker, context deadlines, time.Since anchors).
// Wall clock appears in exactly two places, both unavoidable and both
// offset-bounded: the persisted ExpiresAt (written once by the granting
// node) and the expired-lease takeover test `now > ExpiresAt`. A
// grantor's clock may be at most ~TTL/2 ahead of a taker's for safety;
// NTP keeps real offsets at milliseconds (spec: rates <50%, offsets far
// inside the ≥8s band). A suspended VM resuming with a jumped wall clock
// cannot extend a lease: renewal timers and expiry comparisons against
// the resumed monotonic axis are immune.
package lease

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
)

const (
	// Prefix under which all leases live.
	Prefix = "/leases/"

	// DefaultTTL is the default lease TTL (15 s).
	DefaultTTL = 15 * time.Second

	// RenewInterval is the renewal cadence: TTL/3 (5 s for the default
	// TTL). Renewal is a Raft CAS write; failing or exceeding this
	// budget closes Done() immediately.
	RenewInterval = DefaultTTL / 3
)

// ErrNotAcquired is returned by TryAcquire when the lease is held
// (unexpired) by another holder.
var ErrNotAcquired = errors.New(errors.KindConflict, "lease", "lease held by another holder")

// Lease is the gossiped/store state of one named lease.
type Lease struct {
	Name      string    // "vip:10.43.0.1", "storage-primary:vol-abc"
	Holder    string    // node ID
	Term      uint64    // raft term when granted (0 if unknown)
	ExpiresAt time.Time // granting node's clock
	Revision  store.Revision
}

// leaseValue is the JSON payload stored at /leases/<name>.
type leaseValue struct {
	Holder        string `json:"h"`
	Term          uint64 `json:"t,omitempty"`
	ExpiresAtUnix int64  `json:"e"` // unix-nano, granting node's clock
}

func encodeValue(l Lease) ([]byte, error) {
	b, err := json.Marshal(leaseValue{
		Holder:        l.Holder,
		Term:          l.Term,
		ExpiresAtUnix: l.ExpiresAt.UnixNano(),
	})
	if err != nil {
		return nil, errors.New(errors.KindInternal, "lease.encodeValue", err.Error())
	}
	return b, nil
}

func decodeValue(b []byte) (*leaseValue, error) {
	var v leaseValue
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, errors.New(errors.KindInternal, "lease.decodeValue", "corrupt lease value: "+err.Error())
	}
	return &v, nil
}

// termFunc supplies the current Raft term for granted leases; nil means
// unknown (0). Wired to raft.Term() once the manager runs against a
// raftstore; plain boltstore tests leave it nil.
type termFunc func() uint64

// Manager hands out and renews leases against a Store.
type Manager struct {
	st     store.Store
	nodeID string
	termFn termFunc
	now    func() time.Time // injectable for clock-skew simulation
}

// NewManager returns a lease manager for nodeID backed by st.
func NewManager(st store.Store, nodeID string) *Manager {
	return &Manager{st: st, nodeID: nodeID, now: time.Now}
}

// WithClock overrides the wall-clock source (skew simulation in tests;
// production uses time.Now).
func (m *Manager) WithClock(fn func() time.Time) *Manager {
	m.now = fn
	return m
}

// WithTermFunc attaches a raft-term source (used for fencing metadata).
func (m *Manager) WithTermFunc(fn func() uint64) *Manager {
	m.termFn = fn
	return m
}

func (m *Manager) term() uint64 {
	if m.termFn != nil {
		return m.termFn()
	}
	return 0
}

// Acquire attempts to take a lease; blocks until acquired or ctx done.
func (m *Manager) Acquire(ctx context.Context, name string, ttl time.Duration) (*Held, error) {
	return m.acquire(ctx, name, ttl, false)
}

// AcquireReclaiming is Acquire that also takes back a live record naming this
// node, for holders that must resume at once after a restart. The soundness
// conditions are Maintain's: holder IDs are unique per node, and the caller is not
// a deposed holder whose Abandon()ed record must stay fenced.
func (m *Manager) AcquireReclaiming(ctx context.Context, name string, ttl time.Duration) (*Held, error) {
	return m.acquire(ctx, name, ttl, true)
}

func (m *Manager) acquire(ctx context.Context, name string, ttl time.Duration, reclaimOwn bool) (*Held, error) {
	for {
		h, err := m.tryAcquire(ctx, name, ttl, reclaimOwn)
		if err == nil {
			return h, nil
		}
		if errors.Is(err, errors.KindConflict) {
			// held by someone else (or transient store failure with
			// conflict kind): wait and retry
			if cerr := ctx.Err(); cerr != nil {
				return nil, errors.Wrap(cerr, errors.KindTimeout, "lease.Acquire", "acquire canceled")
			}
			select {
			case <-ctx.Done():
				return nil, errors.Wrap(ctx.Err(), errors.KindTimeout, "lease.Acquire", "acquire canceled")
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}
		// store-level failure (unavailable etc.): retry until ctx done
		if cerr := ctx.Err(); cerr != nil {
			return nil, errors.Wrap(cerr, errors.KindUnavailable, "lease.Acquire", "acquire canceled")
		}
		select {
		case <-ctx.Done():
			return nil, errors.Wrap(ctx.Err(), errors.KindUnavailable, "lease.Acquire", "acquire canceled")
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// Maintain holds name until ctx ends and re-acquires it after any loss (a failed
// renewal during a raft leader change ends a Held for good). Use it for liveness
// records, where being briefly unheld is harmless but staying unheld is not.
// onHeld is called with each newly acquired lease; the lease is released on exit.
//
// Maintain also takes back a record that already names this node: after a lost
// write response the record is ours but no Held exists, and waiting out its TTL
// would read the node as dead. That is only sound where holder IDs are unique per
// node and the name is not fenced by Abandon — true of liveness records.
func (m *Manager) Maintain(ctx context.Context, name string, ttl time.Duration, onHeld func(*Held)) {
	for ctx.Err() == nil {
		h, err := m.acquire(ctx, name, ttl, true)
		if err != nil {
			return // acquire only fails once ctx is done
		}
		onHeld(h)
		select {
		case <-ctx.Done():
			_ = m.Release(context.Background(), h) //nolint:errcheck // best effort on shutdown
			return
		case <-h.Done():
		}
	}
}

// TryAcquire returns immediately: (*Held, nil) on success,
// (nil, ErrNotAcquired) if another live holder owns it, or a store error.
func (m *Manager) TryAcquire(ctx context.Context, name string, ttl time.Duration) (*Held, error) {
	return m.tryAcquire(ctx, name, ttl, false)
}

func (m *Manager) tryAcquire(ctx context.Context, name string, ttl time.Duration, reclaimOwn bool) (*Held, error) {
	if ttl <= 0 {
		return nil, errors.New(errors.KindInvalid, "lease.TryAcquire", "ttl must be positive")
	}
	if ctx.Err() != nil {
		return nil, errors.Wrap(ctx.Err(), errors.KindTimeout, "lease.TryAcquire", "ctx done")
	}
	key := store.Key(Prefix + name)
	now := m.now()

	// Fast path: key must not exist.
	rev, err := m.st.CompareAndSwap(ctx, key, 0, mustEncode(name, m.nodeID, m.term(), now.Add(ttl)))
	if err == nil {
		return m.newHeld(name, m.nodeID, m.term(), now.Add(ttl), rev, ttl), nil
	}
	if !errors.Is(err, errors.KindConflict) {
		return nil, errors.Wrap(err, errors.KindUnavailable, "lease.TryAcquire", "cas create: "+err.Error())
	}

	// Slow path: take over an expired lease. Expiry is judged against
	// the LOCAL clock versus the stored (granting node's) ExpiresAt —
	// the one wall-clock comparison the safety argument tolerates.
	cur, gerr := m.st.Get(ctx, key)
	if gerr != nil {
		return nil, errors.Wrap(gerr, errors.KindUnavailable, "lease.TryAcquire", "get: "+gerr.Error())
	}
	v, derr := decodeValue(cur.Value)
	if derr != nil {
		return nil, derr
	}
	if now.Before(time.Unix(0, v.ExpiresAtUnix)) && !(reclaimOwn && v.Holder == m.nodeID) {
		return nil, ErrNotAcquired // live holder
	}
	rev, err = m.st.CompareAndSwap(ctx, key, cur.Revision, mustEncode(name, m.nodeID, m.term(), now.Add(ttl)))
	if err != nil {
		if errors.Is(err, errors.KindConflict) {
			return nil, ErrNotAcquired // raced; someone else renewed/took
		}
		return nil, errors.Wrap(err, errors.KindUnavailable, "lease.TryAcquire", "cas takeover: "+err.Error())
	}
	return m.newHeld(name, m.nodeID, m.term(), now.Add(ttl), rev, ttl), nil
}

// Inspect returns the stored lease state without acquiring. A
// preferred candidate uses this to time its takeover attempt at the
// recorded expiry instead of polling on a fixed interval, so failover
// lands promptly after the fence expires rather than up to a full
// retry tick later.
func (m *Manager) Inspect(ctx context.Context, name string) (Lease, bool, error) {
	e, err := m.st.Get(ctx, store.Key(Prefix+name))
	if err != nil {
		if errors.Is(err, errors.KindNotFound) {
			return Lease{}, false, nil
		}
		return Lease{}, false, err
	}
	v, derr := decodeValue(e.Value)
	if derr != nil {
		return Lease{}, false, derr
	}
	return Lease{
		Holder: v.Holder, Term: v.Term,
		ExpiresAt: time.Unix(0, v.ExpiresAtUnix), Revision: e.Revision,
	}, true, nil
}

// Release removes the lease key (explicit release closes Done).
func (m *Manager) Release(ctx context.Context, h *Held) error {
	return h.release(ctx, m)
}

// Holder returns the current holder, or "" if none/expired.
func (m *Manager) Holder(ctx context.Context, name string) (string, error) {
	cur, err := m.st.Get(ctx, store.Key(Prefix+name))
	if err != nil {
		if errors.Is(err, errors.KindNotFound) {
			return "", nil
		}
		return "", errors.Wrap(err, errors.KindUnavailable, "lease.Holder", err.Error())
	}
	v, derr := decodeValue(cur.Value)
	if derr != nil {
		return "", derr
	}
	if m.now().After(time.Unix(0, v.ExpiresAtUnix)) {
		return "", nil
	}
	return v.Holder, nil
}

// Inspect returns the stored lease state (expired or not), or
// (nil, nil) when no lease key exists. Read-only — unlike Holder it
// does not judge expiry, so callers can show the full record.
func Inspect(ctx context.Context, st store.Store, name string) (*Lease, error) {
	e, err := st.Get(ctx, store.Key(Prefix+name))
	if errors.Is(err, errors.KindNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.Wrap(err, errors.KindUnavailable, "lease.Inspect", err.Error())
	}
	v, derr := decodeValue(e.Value)
	if derr != nil {
		return nil, derr
	}
	return &Lease{Name: name, Holder: v.Holder, Term: v.Term, ExpiresAt: time.Unix(0, v.ExpiresAtUnix), Revision: e.Revision}, nil
}

func mustEncode(name, holder string, term uint64, expires time.Time) []byte {
	b, err := encodeValue(Lease{Name: name, Holder: holder, Term: term, ExpiresAt: expires})
	if err != nil {
		panic("lease: encode: " + err.Error()) // JSON of a fixed struct cannot fail
	}
	return b
}

// EncodeForTest serializes a Lease exactly as the manager would store it,
// for tests that need to rewrite lease state directly (e.g. forcing
// expiry). Production code must never use this.
func EncodeForTest(l Lease) ([]byte, error) {
	return encodeValue(l)
}

// newHeld starts the background machinery for a freshly acquired lease.
func (m *Manager) newHeld(name, holder string, term uint64, expires time.Time, rev store.Revision, ttl time.Duration) *Held {
	h := &Held{
		Lease: Lease{Name: name, Holder: holder, Term: term, ExpiresAt: expires, Revision: rev},
		mgr:   m,
		ttl:   ttl,
		done:  make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.wg.Add(2)
	go h.renewLoop(ctx)
	go h.watchLoop(ctx)
	return h
}

// Held is a lease in the hands of its holder.
type Held struct {
	Lease
	mgr  *Manager
	ttl  time.Duration
	done chan struct{}

	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	lost    bool
	lostErr error
}

// Done is closed when the lease is lost for ANY reason: expiry, network
// partition, leadership change, explicit release. The holder must stop
// acting within 1 s of the close.
func (h *Held) Done() <-chan struct{} { return h.done }

// Abandon stops acting as the holder WITHOUT deleting the record: the
// renewal/watch loops stop and Done closes, but the stored lease is
// left to expire naturally (≤ TTL). Use when the holder's authority
// ended but the lease itself must not be yanked — e.g. a block
// controller deposed as raft leader keeps its singleton placements
// fenced until the new leader's takeover CAS succeeds after expiry
// (Phase 04 T25: a zombie renewal through write forwarding would
// otherwise pin the lease forever and stall singleton replacement).
func (h *Held) Abandon() {
	h.cancel()
	h.markLost(errors.New(errors.KindUnavailable, "lease", "abandoned by its holder"))
}

// Valid reports whether the lease is currently believed held.
func (h *Held) Valid() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.lost
}

// Err reports why the lease was lost, or nil while it is held.
func (h *Held) Err() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lostErr
}

// CurrentLease returns a snapshot of the lease state (the embedded
// Lease fields are updated by the renewal loop and must be read through
// this method, not directly, once the Held is live).
func (h *Held) CurrentLease() Lease {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.Lease
}

// markLost records why the lease was lost and closes Done, both exactly once.
func (h *Held) markLost(why error) {
	h.mu.Lock()
	already := h.lost
	if !already {
		h.lost, h.lostErr = true, why
	}
	h.mu.Unlock()
	if !already {
		close(h.done)
	}
}

func (h *Held) renewLoop(ctx context.Context) {
	defer h.wg.Done()
	// Monotonic ticker (time.Ticker uses the monotonic clock; a wall
	// clock jump does not accelerate or stall it — spec §10).
	t := time.NewTicker(h.ttl / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if !h.renewOnce(ctx) {
			return
		}
	}
}

// renewOnce performs one CAS renewal with a hard budget of ttl/3
// (monotonic context deadline). Returns false when the lease is lost.
func (h *Held) renewOnce(ctx context.Context) bool {
	// A lease whose Done() already closed must never be renewed: the
	// holder has (or is about to) stop acting, and a zombie renewal
	// would steal the key back from its legitimate new owner. The CAS
	// revision check below independently prevents overwrites.
	if !h.Valid() {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, h.ttl/3)
	defer cancel()
	key := store.Key(Prefix + h.Lease.Name)
	expires := h.mgr.now().Add(h.ttl)
	start := time.Now()
	rev, err := h.mgr.st.CompareAndSwap(cctx, key, h.Lease.Revision,
		mustEncode(h.Lease.Name, h.Lease.Holder, h.mgr.term(), expires))
	if err != nil {
		// Renewal failed (conflict, unavailability, or budget exceeded).
		// Close Done() immediately — do not wait for expiry.
		h.markLost(errors.Wrap(err, errors.KindUnavailable, "lease.renew",
			fmt.Sprintf("renewal failed after %v: %v", time.Since(start).Round(time.Millisecond), err)))
		return false
	}
	h.mu.Lock()
	h.Lease.Revision = rev
	h.Lease.ExpiresAt = expires
	h.mu.Unlock()
	return true
}

// watchLoop observes the lease key. Losing the watch stream (partition,
// store shutdown) or seeing the key change hands closes Done — the
// liveness signal arrives with gossip-level latency instead of waiting
// for the next renewal tick.
func (h *Held) watchLoop(ctx context.Context) {
	defer h.wg.Done()
	events, err := h.mgr.st.Watch(ctx, store.Key(Prefix+h.Lease.Name), h.Lease.Revision)
	if err != nil {
		h.markLost(errors.Wrap(err, errors.KindUnavailable, "lease.watch", "watch failed: "+err.Error()))
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				// Stream closed by partition or store shutdown:
				// conservatively treat the lease as lost unless this is
				// our own shutdown after an explicit release/loss.
				h.markLost(errors.New(errors.KindUnavailable, "lease.watch", "watch stream closed"))
				return
			}
			switch ev.Type {
			case store.EventDelete:
				h.markLost(errors.New(errors.KindConflict, "lease.watch", "watch saw the record deleted"))
				return
			case store.EventPut:
				v, derr := decodeValue(ev.Entry.Value)
				if derr != nil || v.Holder != h.Lease.Holder {
					h.markLost(errors.New(errors.KindConflict, "lease.watch", "watch saw the record taken over or corrupted"))
					return
				}
			}
		}
	}
}

// release stops the background loops, marks the lease lost (closing
// Done), and best-effort deletes the key.
func (h *Held) release(ctx context.Context, m *Manager) error {
	h.markLost(errors.New(errors.KindUnavailable, "lease", "released by its holder"))
	h.cancel()
	h.wg.Wait()
	err := m.st.Delete(ctx, store.Key(Prefix+h.Lease.Name), h.Lease.Revision)
	if err != nil && errors.Is(err, errors.KindNotFound) {
		return nil // already gone (expired and taken over): fine
	}
	if err != nil {
		return errors.Wrap(err, errors.KindUnavailable, "lease.Release", err.Error())
	}
	return nil
}

// Guard runs fn while holding the named lease, fencing it: fn's context
// is canceled the moment the lease is lost, so the protected work stops
// promptly. Nobody should hand-write this pattern (spec §4.3).
func Guard(ctx context.Context, m *Manager, name string, ttl time.Duration, fn func(context.Context) error) error {
	h, err := m.Acquire(ctx, name, ttl)
	if err != nil {
		return err
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = m.Release(rctx, h)
	}()

	fctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-h.Done():
			cancel() // fence: cancel work when lease lost
		case <-fctx.Done():
		}
	}()
	return fn(fctx)
}

// String renders the lease for logs.
func (l Lease) String() string {
	return fmt.Sprintf("%s held by %s (term %d, rev %d)", l.Name, l.Holder, l.Term, l.Revision)
}
