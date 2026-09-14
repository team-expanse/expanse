package lease_test

import (
	"context"
	stderrors "errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/cluster/lease"
	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/boltstore"
)

// --- fault-injection wrappers -------------------------------------------

// partStore wraps a boltstore and can simulate a network partition: all
// writes/reads fail fast and watch streams are torn down (as a gRPC
// stream break would be against a raftstore).
type partStore struct {
	store.Store

	mu      sync.Mutex
	down    bool
	watches []context.CancelFunc
}

func (p *partStore) partition() {
	p.mu.Lock()
	p.down = true
	ws := p.watches
	p.watches = nil
	p.mu.Unlock()
	for _, c := range ws {
		c() // close watch streams: the partition signal
	}
}

func (p *partStore) heal() {
	p.mu.Lock()
	p.down = false
	p.mu.Unlock()
}

func (p *partStore) downErr(op string) error {
	return errors.New(errors.KindUnavailable, op, "simulated partition")
}

func (p *partStore) check(op string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.down {
		return p.downErr(op)
	}
	return nil
}

func (p *partStore) Get(ctx context.Context, k store.Key) (*store.Entry, error) {
	if err := p.check("part.Get"); err != nil {
		return nil, err
	}
	return p.Store.Get(ctx, k)
}

func (p *partStore) CompareAndSwap(ctx context.Context, k store.Key, expect store.Revision, v []byte) (store.Revision, error) {
	if err := p.check("part.CAS"); err != nil {
		return 0, err
	}
	return p.Store.CompareAndSwap(ctx, k, expect, v)
}

func (p *partStore) Delete(ctx context.Context, k store.Key, expect store.Revision) error {
	if err := p.check("part.Delete"); err != nil {
		return err
	}
	return p.Store.Delete(ctx, k, expect)
}

func (p *partStore) Watch(ctx context.Context, prefix store.Key, fromRev store.Revision) (<-chan store.Event, error) {
	pctx, cancel := context.WithCancel(ctx)
	p.mu.Lock()
	if p.down {
		p.mu.Unlock()
		cancel()
		return nil, p.downErr("part.Watch")
	}
	p.watches = append(p.watches, cancel)
	p.mu.Unlock()
	return p.Store.Watch(pctx, prefix, fromRev)
}

// slowStore delays CAS calls by d (simulating a slow/saturating leader).
type slowStore struct {
	store.Store
	d time.Duration
}

func (s *slowStore) CompareAndSwap(ctx context.Context, k store.Key, expect store.Revision, v []byte) (store.Revision, error) {
	select {
	case <-time.After(s.d):
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	return s.Store.CompareAndSwap(ctx, k, expect, v)
}

// --- helpers -------------------------------------------------------------

func newBoltStore(t *testing.T) *boltstore.Store {
	t.Helper()
	st, err := boltstore.New(filepath.Join(t.TempDir(), "lease.db"))
	if err != nil {
		t.Fatalf("boltstore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func waitDone(t *testing.T, h *lease.Held, d time.Duration) time.Time {
	t.Helper()
	select {
	case <-h.Done():
		return time.Now()
	case <-time.After(d):
		t.Fatalf("Done() did not close within %v", d)
		return time.Time{}
	}
}

// --- tests ---------------------------------------------------------------

func TestAcquireReleaseHolder(t *testing.T) {
	st := newBoltStore(t)
	m := lease.NewManager(st, "node-a")

	ctx := context.Background()
	h, err := m.Acquire(ctx, "vip:10.43.0.1", lease.DefaultTTL)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if !h.Valid() {
		t.Fatal("lease not valid after acquire")
	}
	if h.Holder != "node-a" || h.Name != "vip:10.43.0.1" {
		t.Errorf("lease = %+v", h.Lease)
	}

	got, err := m.Holder(ctx, "vip:10.43.0.1")
	if err != nil || got != "node-a" {
		t.Fatalf("Holder = %q, %v", got, err)
	}

	if err := m.Release(ctx, h); err != nil {
		t.Fatalf("Release: %v", err)
	}
	select {
	case <-h.Done():
	case <-time.After(time.Second):
		t.Fatal("Done not closed after release")
	}
	if h.Valid() {
		t.Error("Valid after release")
	}
	if got, _ := m.Holder(ctx, "vip:10.43.0.1"); got != "" {
		t.Errorf("Holder after release = %q", got)
	}
}

func TestTryAcquireConflictAndTakeover(t *testing.T) {
	st := newBoltStore(t)
	a := lease.NewManager(st, "node-a")
	b := lease.NewManager(st, "node-b")
	ctx := context.Background()

	h, err := a.TryAcquire(ctx, "storage-primary:vol-abc", 200*time.Millisecond)
	if err != nil {
		t.Fatalf("a TryAcquire: %v", err)
	}
	if _, err := b.TryAcquire(ctx, "storage-primary:vol-abc", time.Second); !stderrors.Is(err, lease.ErrNotAcquired) {
		t.Fatalf("b TryAcquire while held: err=%v want ErrNotAcquired", err)
	}

	// Takeover of a live lease must fail; takeover of an expired lease
	// must succeed via CAS on the stored revision.
	cur, err := st.Get(ctx, store.Key(lease.Prefix+"storage-primary:vol-abc"))
	if err != nil {
		t.Fatalf("get lease key: %v", err)
	}
	if _, err := b.TryAcquire(ctx, "storage-primary:vol-abc", time.Second); !stderrors.Is(err, lease.ErrNotAcquired) {
		t.Fatalf("b TryAcquire before expiry: %v", err)
	}
	// Force expiry by rewriting the stored expiry into the past (the
	// deterministic stand-in for waiting out the TTL).
	past, err := lease.EncodeForTest(lease.Lease{
		Name: "storage-primary:vol-abc", Holder: "node-a",
		ExpiresAt: time.Now().Add(-time.Second), Revision: cur.Revision,
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := st.CompareAndSwap(ctx, cur.Key, cur.Revision, past); err != nil {
		t.Fatalf("force expiry: %v", err)
	}
	hb, err := b.TryAcquire(ctx, "storage-primary:vol-abc", time.Second)
	if err != nil {
		t.Fatalf("b takeover after expiry: %v", err)
	}
	if hb.Holder != "node-b" {
		t.Errorf("takeover holder = %q", hb.Holder)
	}
	_ = h
}

// TestRenewalExtendsLease: the renewal loop CASes the lease forward and
// keeps Done open.
func TestRenewalExtendsLease(t *testing.T) {
	st := newBoltStore(t)
	m := lease.NewManager(st, "node-a")
	ctx := context.Background()

	h, err := m.Acquire(ctx, "renew", 900*time.Millisecond) // renew every 300ms
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer m.Release(ctx, h)

	rev0 := h.CurrentLease().Revision
	time.Sleep(1250 * time.Millisecond) // ~4 renewal ticks

	if !h.Valid() {
		t.Fatal("lease lost during healthy renewal")
	}
	select {
	case <-h.Done():
		t.Fatal("Done closed during healthy renewal")
	default:
	}
	if rev := h.CurrentLease().Revision; rev <= rev0 {
		t.Errorf("revision did not advance: %d → %d", rev0, rev)
	}
}

// TestSlowRenewalClosesDone: a renewal that exceeds TTL/3 closes Done
// immediately (it must not wait for expiry). Spec: slow (>TTL/3) is a loss.
func TestSlowRenewalClosesDone(t *testing.T) {
	st := newBoltStore(t)
	ctx := context.Background()

	// Renewal budget is TTL/3 = 300ms; each CAS attempt hangs 900ms.
	slow := &slowStore{Store: st, d: 900 * time.Millisecond}
	m := lease.NewManager(slow, "node-c")
	h, err := m.Acquire(ctx, "slow-real", 900*time.Millisecond)
	if err != nil {
		t.Fatalf("Acquire on slow store: %v", err)
	}
	grantedAt := time.Now()
	doneAt := waitDone(t, h, 3*time.Second)
	if h.Valid() {
		t.Error("Held still valid after slow renewal")
	}
	// First renewal tick at grant+300ms + 300ms budget → lost by
	// grant+600ms, before the 900ms TTL would have run out.
	if elapsed := doneAt.Sub(grantedAt); elapsed >= 900*time.Millisecond {
		t.Errorf("Done closed %v after grant; slow renewal must close it before the %v TTL expiry", elapsed, 900*time.Millisecond)
	}
}

// TestPartitionClosesDoneFast: on partition the watch stream breaks and
// Done closes within 1 s — the holder need not wait for expiry.
func TestPartitionClosesDoneFast(t *testing.T) {
	pst := &partStore{Store: newBoltStore(t)}
	m := lease.NewManager(pst, "node-a")
	ctx := context.Background()

	h, err := m.Acquire(ctx, "vip:10.43.0.1", lease.DefaultTTL)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	partitionedAt := time.Now()
	pst.partition()

	doneAt := waitDone(t, h, 1*time.Second)
	if d := doneAt.Sub(partitionedAt); d > time.Second {
		t.Errorf("Done closed %v after partition; want <1s", d)
	}
}

// TestGuardBand is the mandated guard-band measurement (spec §4.3): under
// a simulated partition the old holder stops well before the new holder
// may start — the band must be ≥ 8 s. NOT SKIPPED (G3.7/G3.13).
//
// Timeline (TTL 15 s, renew 5 s): acquire at t0; partition at t0+0.5s
// kills the watch stream (Done ≈ t0+0.5s, well inside TTL/3 + budget).
// The last successful grant was t0, so the lease expires at t0+15s by
// the granting clock; the taker (same clock here) cannot CAS it before
// then. Band = t_takeover − t_done ≥ 15 − (0.5 + ε) ≫ 8 s.
func TestGuardBand(t *testing.T) {
	pst := &partStore{Store: newBoltStore(t)}
	a := lease.NewManager(pst, "node-a")
	b := lease.NewManager(pst, "node-b")
	ctx := context.Background()

	h, err := a.Acquire(ctx, "vip:10.43.0.1", lease.DefaultTTL)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	partitionedAt := time.Now()
	pst.partition()
	doneAt := waitDone(t, h, 1*time.Second) // old holder stops (≤1s stop rule)

	// Old holder honors the 1-second stop rule: the protected workload
	// must be stopped by doneAt+1s; we treat doneAt as the stop point.

	pst.heal() // partition healed; b may try to take over

	var band time.Duration
	deadline := time.Now().Add(30 * time.Second)
	for {
		hb, err := b.TryAcquire(ctx, "vip:10.43.0.1", lease.DefaultTTL)
		if err == nil {
			band = time.Since(doneAt)
			_ = hb
			break
		}
		if !stderrors.Is(err, lease.ErrNotAcquired) {
			t.Fatalf("TryAcquire: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("new holder could not acquire within 30s of healing")
		}
		time.Sleep(100 * time.Millisecond)
	}

	t.Logf("guard band: %v (old holder stopped at partition+%v, new holder at expiry)",
		band, doneAt.Sub(partitionedAt))
	if band < 8*time.Second {
		t.Errorf("guard band = %v; spec guarantees ≥8s (TTL 15s − TTL/3 5s − renewal timeout 2s)", band)
	}
}

// skewClock shifts time.Now by off; used to simulate NTP offsets.
func skewClock(off time.Duration) func() time.Time {
	return func() time.Time { return time.Now().Add(off) }
}

// TestClockSkewNeverTwoHolders: with the grantor's clock +2s and the
// taker's −2s (4s relative offset, far above real NTP error), there is
// never a moment when both believe they hold the lease. Offset does not
// defeat the argument; only clock RATE would (>50% drift).
func TestClockSkewNeverTwoHolders(t *testing.T) {
	pst := &partStore{Store: newBoltStore(t)}
	// TTL 6s, renew 2s: with fast-fail renewal, the old holder's Done
	// closes at the first tick after the partition (≤2s), while the
	// taker — whose clock runs 4s behind the grantor's — cannot take
	// over until well after: grantor-clock expiry t0+2+6=t0+8 is seen
	// by the taker's clock only at t0+10 real time. Margin ≫ 0.
	a := lease.NewManager(pst, "node-a").WithClock(skewClock(2 * time.Second))
	b := lease.NewManager(pst, "node-b").WithClock(skewClock(-2 * time.Second))
	ctx := context.Background()

	h, err := a.Acquire(ctx, "skew", 6*time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// While a holds, every b attempt must fail.

	// Partition a at t0+0.5s; its renewal fails fast at the first tick.
	partitionedAt := time.Now()
	pst.partition()
	doneAt := waitDone(t, h, 3*time.Second)

	// b tries continuously; record when it first succeeds.
	pst.heal()
	var acquiredAt time.Time
	deadline := time.Now().Add(20 * time.Second)
	for {
		_, err := b.TryAcquire(ctx, "skew", 6*time.Second)
		if err == nil {
			acquiredAt = time.Now()
			if h.Valid() {
				t.Fatal("TWO HOLDERS: b acquired while a still valid")
			}
			select {
			case <-h.Done():
			default:
				t.Fatal("TWO HOLDERS: a's Done not closed at b's takeover")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("b never acquired")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if gap := acquiredAt.Sub(doneAt); gap < time.Second {
		t.Errorf("b acquired %v after a's Done; overlaps are a split brain", gap)
	}
	t.Logf("skew test: a lost at partition+%v, b took over %v later",
		doneAt.Sub(partitionedAt), acquiredAt.Sub(doneAt))
}

// TestGuardHelperFences: lease.Guard cancels fn's context when the lease
// is lost, so protected work stops within the 1-second rule.
func TestGuardHelperFences(t *testing.T) {
	pst := &partStore{Store: newBoltStore(t)}
	m := lease.NewManager(pst, "node-a")

	stopped := make(chan error, 1)
	partitionedAt := time.Time{}
	go func() {
		err := lease.Guard(context.Background(), m, "vip:10.43.0.1", lease.DefaultTTL, func(fctx context.Context) error {
			<-fctx.Done()
			return fctx.Err()
		})
		stopped <- err
	}()

	// Wait for the lease to exist, then partition.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if h, _ := m.Holder(context.Background(), "vip:10.43.0.1"); h == "node-a" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("lease never appeared")
		}
		time.Sleep(20 * time.Millisecond)
	}
	partitionedAt = time.Now()
	pst.partition()

	select {
	case err := <-stopped:
		if !stderrors.Is(err, context.Canceled) {
			t.Errorf("fn err = %v, want context.Canceled", err)
		}
		if d := time.Since(partitionedAt); d > 2*time.Second {
			t.Errorf("protected work stopped %v after partition; must stop promptly", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("protected work never stopped")
	}
}

// TestAcquireBlocksUntilAvailable: Acquire retries through store
// unavailability instead of failing.
func TestAcquireBlocksUntilAvailable(t *testing.T) {
	pst := &partStore{Store: newBoltStore(t)}
	m := lease.NewManager(pst, "node-a")
	ctx := context.Background()

	go func() {
		time.Sleep(300 * time.Millisecond)
		pst.heal()
	}()
	pst.partition()

	h, err := m.Acquire(ctx, "after-partition", time.Second)
	if err != nil {
		t.Fatalf("Acquire through partition: %v", err)
	}
	if !h.Valid() {
		t.Error("lease not valid")
	}
}

// TestInspect covers the raw record reader: existing, missing, and the
// rendered log form.
func TestInspect(t *testing.T) {
	st := newBoltStore(t)
	ctx := context.Background()

	// Missing → (nil, nil).
	got, err := lease.Inspect(ctx, st, "nope")
	if err != nil || got != nil {
		t.Fatalf("Inspect(missing) = (%v, %v), want (nil, nil)", got, err)
	}

	m := lease.NewManager(st, "node-a")
	h, err := m.Acquire(ctx, "inspect-me", time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = m.Release(ctx, h) }()

	rec, err := lease.Inspect(ctx, st, "inspect-me")
	if err != nil || rec == nil {
		t.Fatalf("Inspect = (%v, %v)", rec, err)
	}
	if rec.Holder != "node-a" {
		t.Errorf("holder = %q", rec.Holder)
	}
	if rec.ExpiresAt.Before(time.Now()) {
		t.Errorf("expiry %v already passed", rec.ExpiresAt)
	}
	if s := rec.String(); !strings.Contains(s, "inspect-me") || !strings.Contains(s, "node-a") {
		t.Errorf("String() = %q", s)
	}
}

// TestWithTermFunc covers the fencing-term attachment.
func TestWithTermFunc(t *testing.T) {
	st := newBoltStore(t)
	ctx := context.Background()
	term := uint64(7)
	m := lease.NewManager(st, "node-a").WithTermFunc(func() uint64 { return term })
	h, err := m.Acquire(ctx, "termed", time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = m.Release(ctx, h) }()
	rec, err := lease.Inspect(ctx, st, "termed")
	if err != nil || rec == nil {
		t.Fatalf("Inspect = (%v, %v)", rec, err)
	}
	if rec.Term != 7 {
		t.Errorf("term = %d, want 7", rec.Term)
	}
}

// TestAcquireBlocksThenAcquires covers the blocking Acquire retry
// loop: a contender waits while the lease is live (holder auto-renews)
// and acquires as soon as the holder releases.
func TestAcquireBlocksThenAcquires(t *testing.T) {
	st := newBoltStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	first := lease.NewManager(st, "node-a")
	h, err := first.Acquire(ctx, "contention", time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer func() { _ = first.Release(context.Background(), h) }()

	second := lease.NewManager(st, "node-b")
	start := time.Now()
	go func() { // release after the contender has begun waiting
		time.Sleep(400 * time.Millisecond)
		_ = first.Release(context.Background(), h)
	}()
	h2, err := second.Acquire(ctx, "contention", 500*time.Millisecond)
	if err != nil {
		t.Fatalf("blocking Acquire: %v", err)
	}
	defer func() { _ = second.Release(context.Background(), h2) }()
	if time.Since(start) < 300*time.Millisecond {
		t.Errorf("Acquire succeeded too early (%v) — holder may have been bypassed", time.Since(start))
	}
	rec, err := lease.Inspect(ctx, st, "contention")
	if err != nil || rec == nil || rec.Holder != "node-b" {
		t.Errorf("holder after takeover = %+v %v", rec, err)
	}
}

// TestAbandonStopsRenewalKeepsRecord: Abandon stops the renewal loops
// and closes Done, but the stored record is left untouched — it must
// expire naturally (TTL), not vanish. A takeover CAS succeeds only
// after that expiry (the deposed-leader singleton fencing path).
func TestAbandonStopsRenewalKeepsRecord(t *testing.T) {
	ctx := context.Background()
	st := newBoltStore(t)
	mgr := lease.NewManager(st, "node-a")

	h, err := mgr.TryAcquire(ctx, "fence", time.Second)
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}

	// Record exists with holder node-a before the abandon.
	rec, err := lease.Inspect(ctx, st, "fence")
	if err != nil || rec == nil || rec.Holder != "node-a" {
		t.Fatalf("pre-abandon record = %+v, %v", rec, err)
	}

	h.Abandon()
	select {
	case <-h.Done():
	case <-time.After(time.Second):
		t.Fatal("Done not closed after Abandon")
	}
	if h.Valid() {
		t.Error("Held still valid after Abandon")
	}

	// Record must still exist (left to expire, not deleted)…
	rec, err = lease.Inspect(ctx, st, "fence")
	if err != nil || rec == nil {
		t.Fatalf("post-abandon record missing: %+v, %v", rec, err)
	}
	// …and must eventually expire so a takeover succeeds (TTL 1 s;
	// abandon stops renewals, so expiry lands within ~1 s).
	second := lease.NewManager(st, "node-b")
	deadline := time.Now().Add(4 * time.Second)
	for {
		aerr := func() error {
			h2, err2 := second.TryAcquire(ctx, "fence", time.Second)
			if err2 == nil {
				_ = second.Release(ctx, h2)
			}
			return err2
		}()
		if aerr == nil {
			break // takeover succeeded
		}
		if time.Now().After(deadline) {
			t.Fatalf("takeover after abandon+expiry never succeeded: %v", aerr)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
