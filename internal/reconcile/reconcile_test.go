package reconcile

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/boltstore"
)

func testStore(t *testing.T) store.Store {
	t.Helper()
	s, err := boltstore.New(filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(testWriter{}, nil))
}

type testWriter struct{}

func (testWriter) Write(p []byte) (int, error) { return len(p), nil }

// ---- fake manager ----

type fakeResource struct {
	id   string
	typ  string
	deps []string
}

func (f *fakeResource) ID() string             { return f.id }
func (f *fakeResource) Type() string           { return f.typ }
func (f *fakeResource) Dependencies() []string { return f.deps }

type fakeManager struct {
	mu sync.Mutex
	// behavior knobs
	observeDelay time.Duration
	applyDelay   time.Duration
	failObserve  bool
	failApply    bool

	// records
	observeOrder []string
	converged    map[string]bool
	applyCount   atomic.Int64
	concurrent   atomic.Int64
	maxConcurr   atomic.Int64
}

func newFakeManager() *fakeManager {
	return &fakeManager{converged: map[string]bool{}}
}

func (m *fakeManager) Type() string { return "fake" }

func (m *fakeManager) Load(id string, spec []byte) (Resource, error) {
	return &fakeResource{id: id, typ: "fake"}, nil
}

func (m *fakeManager) Observe(ctx context.Context, r Resource) (Observed, error) {
	cur := m.concurrent.Add(1)
	for {
		old := m.maxConcurr.Load()
		if cur <= old || m.maxConcurr.CompareAndSwap(old, cur) {
			break
		}
	}
	defer m.concurrent.Add(-1)
	m.mu.Lock()
	m.observeOrder = append(m.observeOrder, r.ID())
	m.mu.Unlock()
	if m.observeDelay > 0 {
		select {
		case <-time.After(m.observeDelay):
		case <-ctx.Done():
			return Observed{}, ctx.Err()
		}
	}
	if m.failObserve {
		return Observed{}, fmt.Errorf("injected observe failure")
	}
	m.mu.Lock()
	conv := m.converged[r.ID()]
	m.mu.Unlock()
	if conv {
		return Observed{Exists: true, InSync: true, Health: HealthHealthy}, nil
	}
	return Observed{Exists: false, InSync: false, Health: HealthUnknown}, nil
}

func (m *fakeManager) Plan(ctx context.Context, r Resource, o Observed) ([]Action, error) {
	if o.InSync {
		return nil, nil
	}
	return []Action{{
		ResourceID:  r.ID(),
		Kind:        "create",
		Description: "create " + r.ID(),
		Fn:          func(ctx context.Context) error { return nil },
	}}, nil
}

func (m *fakeManager) Apply(ctx context.Context, a Action) error {
	m.applyCount.Add(1)
	if m.applyDelay > 0 {
		select {
		case <-time.After(m.applyDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if m.failApply {
		return fmt.Errorf("injected apply failure")
	}
	m.mu.Lock()
	m.converged[a.ResourceID] = true
	m.mu.Unlock()
	return nil
}

func (m *fakeManager) observedOrder() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.observeOrder...)
}

func putDesired(t *testing.T, s store.Store, nodeID, id string, deps string) {
	t.Helper()
	spec := "type: fake\n"
	if deps != "" {
		spec += "deps: " + deps + "\n"
	}
	if _, err := s.Put(context.Background(),
		store.Key(fmt.Sprintf("/node/%s/resources/%s", nodeID, id)), []byte(spec)); err != nil {
		t.Fatal(err)
	}
}

// The fake Load above ignores deps in the spec; use a dep-aware loader.
type depManager struct {
	fakeManager
	deps map[string][]string
}

func (m *depManager) Load(id string, spec []byte) (Resource, error) {
	return &fakeResource{id: id, typ: "fake", deps: m.deps[id]}, nil
}

func newDepReconciler(t *testing.T, deps map[string][]string) (*Reconciler, *depManager) {
	s := testStore(t)
	m := &depManager{fakeManager: fakeManager{converged: map[string]bool{}}, deps: deps}
	r := New(s, Options{NodeID: "n1", Logger: testLogger(), Period: time.Hour})
	r.Register(m)
	return r, m
}

// ---- tests ----

func TestTopoSortOrdersDependenciesFirst(t *testing.T) {
	r, m := newDepReconciler(t, map[string][]string{
		"a": {},
		"b": {"a"},
		"c": {"b"},
		"d": {"a"},
	})
	for _, id := range []string{"c", "d", "b", "a"} {
		putDesired(t, r.store, "n1", id, "")
	}
	if err := r.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	order := m.observedOrder()
	pos := map[string]int{}
	for i, id := range order {
		pos[id] = i
	}
	if pos["a"] > pos["b"] || pos["b"] > pos["c"] || pos["a"] > pos["d"] {
		t.Errorf("dependency order violated: %v", order)
	}
}

func TestTopoSortDetectsCycle(t *testing.T) {
	r, _ := newDepReconciler(t, map[string][]string{
		"a": {"b"},
		"b": {"c"},
		"c": {"a"},
	})
	for _, id := range []string{"a", "b", "c"} {
		putDesired(t, r.store, "n1", id, "")
	}
	err := r.Tick(context.Background())
	if err == nil {
		t.Fatal("expected cycle error")
	}
}

func TestIndependentResourcesRunInParallel(t *testing.T) {
	deps := map[string][]string{}
	for i := 0; i < 4; i++ {
		deps[fmt.Sprintf("r%d", i)] = nil
	}
	r, m := newDepReconciler(t, deps)
	m.observeDelay = 100 * time.Millisecond
	for i := 0; i < 4; i++ {
		putDesired(t, r.store, "n1", fmt.Sprintf("r%d", i), "")
	}
	start := time.Now()
	if err := r.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	if took > 300*time.Millisecond {
		t.Errorf("tick took %v; expected parallel execution (4x100ms serialized would be ~400ms)", took)
	}
	if m.maxConcurr.Load() < 2 {
		t.Errorf("max concurrency = %d, want >= 2", m.maxConcurr.Load())
	}
}

func TestDependencyChainIsSerial(t *testing.T) {
	deps := map[string][]string{
		"a": {},
		"b": {"a"},
		"c": {"b"},
	}
	r, m := newDepReconciler(t, deps)
	m.observeDelay = 50 * time.Millisecond
	for _, id := range []string{"a", "b", "c"} {
		putDesired(t, r.store, "n1", id, "")
	}
	if err := r.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.maxConcurr.Load() != 1 {
		t.Errorf("max concurrency = %d, want 1 for a dependency chain", m.maxConcurr.Load())
	}
}

func TestIdempotencySecondTickZeroChanges(t *testing.T) {
	r, _ := newDepReconciler(t, map[string][]string{"a": {}})
	putDesired(t, r.store, "n1", "a", "")
	if err := r.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	m1 := r.Metrics()
	if m1.ChangesApplied == 0 {
		t.Fatalf("first tick should apply changes, got %d", m1.ChangesApplied)
	}
	if err := r.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	m2 := r.Metrics()
	if m2.ChangesApplied != m1.ChangesApplied {
		t.Errorf("second tick applied %d new changes; idempotency requires 0",
			m2.ChangesApplied-m1.ChangesApplied)
	}
}

func TestBackoffScheduleExactness(t *testing.T) {
	cap30 := 30 * time.Second
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30, 30}
	want = durationSeconds(want)
	for n, w := range want {
		if got := BackoffBase(n+1, cap30); got != w {
			t.Errorf("BackoffBase(%d) = %v, want %v", n+1, got, w)
		}
	}
}

func durationSeconds(secs []time.Duration) []time.Duration {
	out := make([]time.Duration, len(secs))
	for i, s := range secs {
		out[i] = s * time.Second
	}
	return out
}

func TestFailedResourceBacksOff(t *testing.T) {
	r, m := newDepReconciler(t, map[string][]string{"bad": {}})
	m.failApply = true
	putDesired(t, r.store, "n1", "bad", "")
	if err := r.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.applyCount.Load() != 1 {
		t.Fatalf("first tick apply count = %d, want 1", m.applyCount.Load())
	}
	// Second tick immediately: resource must be in backoff, not re-applied.
	if err := r.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := m.applyCount.Load(); got != 1 {
		t.Errorf("second immediate tick re-applied (count=%d); backoff must defer", got)
	}
	// After success (failures cleared), it applies again.
	m.mu.Lock()
	m.failApply = false
	m.mu.Unlock()
	r.markSuccess("bad")
	if err := r.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := m.applyCount.Load(); got != 2 {
		t.Errorf("post-backoff tick apply count = %d, want 2", got)
	}
}

func TestObserveTimeoutEnforced(t *testing.T) {
	r, m := newDepReconciler(t, map[string][]string{"slow": {}})
	m.observeDelay = 5 * time.Second
	r.opts.ObserveTimeout = 50 * time.Millisecond
	putDesired(t, r.store, "n1", "slow", "")
	start := time.Now()
	if err := r.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("tick took %v; observe timeout not enforced", took)
	}
	entries, err := r.store.List(context.Background(), r.StatusPrefix())
	if err != nil || len(entries) != 1 {
		t.Fatalf("status not recorded: %v %d", err, len(entries))
	}
	if want := "observe: context deadline exceeded"; !contains(string(entries[0].Value), want) {
		t.Errorf("status = %q, want observe-timeout error recorded", entries[0].Value)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestDryRunAppliesNothing(t *testing.T) {
	s := testStore(t)
	m := newFakeManager()
	r := New(s, Options{NodeID: "n1", Logger: testLogger(), DryRun: true})
	r.Register(m)
	putDesired(t, s, "n1", "a", "")
	if err := r.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.applyCount.Load() != 0 {
		t.Errorf("dry-run applied %d actions, want 0", m.applyCount.Load())
	}
}

func TestWatchTriggerCausesTick(t *testing.T) {
	s := testStore(t)
	m := newFakeManager()
	r := New(s, Options{NodeID: "n1", Logger: testLogger(), Period: time.Hour, Debounce: 20 * time.Millisecond})
	r.Register(m)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	// Wait for the startup tick to complete, then write desired state:
	// the watch must fire and trigger a debounced second tick.
	deadline := time.After(5 * time.Second)
	for r.Metrics().Ticks == 0 {
		select {
		case <-deadline:
			t.Fatal("startup tick did not run")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	time.Sleep(50 * time.Millisecond) // let the watch registration settle
	putDesired(t, s, "n1", "a", "")   // must trigger a debounced tick
	deadline = time.After(5 * time.Second)
	for r.Metrics().ChangesApplied == 0 {
		select {
		case <-deadline:
			t.Fatal("watch-triggered tick did not run within 5s")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
}

func TestUnknownTypeRecordedAsUnhealthy(t *testing.T) {
	s := testStore(t)
	r := New(s, Options{NodeID: "n1", Logger: testLogger()})
	// No manager registered at all.
	if _, err := s.Put(context.Background(), store.Key("/node/n1/resources/x"),
		[]byte("type: file\npath: /etc/motd")); err != nil {
		t.Fatal(err)
	}
	if err := r.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	entries, err := s.List(context.Background(), r.StatusPrefix())
	if err != nil || len(entries) != 1 {
		t.Fatalf("status entries: %v %d", err, len(entries))
	}
	if !contains(string(entries[0].Value), "no manager") {
		t.Errorf("status = %q, want unknown-type error", entries[0].Value)
	}
}

// TestFreezeSkipsTicks (§4.10.4): a frozen reconciler applies nothing
// new — desired state written while frozen is left unapplied; existing
// applied state is untouched; unfreezing converges.
func TestFreezeSkipsTicks(t *testing.T) {
	s := testStore(t)
	m := newFakeManager()
	r := New(s, Options{NodeID: "n1", Logger: testLogger(), Period: time.Hour})
	r.Register(m)

	// Freeze BEFORE any desired state exists.
	r.Freeze(true)
	if !r.Frozen() {
		t.Fatal("not frozen after Freeze(true)")
	}
	putDesired(t, s, "n1", "a", "")
	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("tick while frozen: %v", err)
	}
	if m.applyCount.Load() != 0 {
		t.Fatalf("frozen tick applied %d actions, want 0", m.applyCount.Load())
	}

	// Unfreeze: the desired state converges on the next tick.
	r.Freeze(false)
	if r.Frozen() {
		t.Fatal("still frozen after Freeze(false)")
	}
	if err := r.Tick(context.Background()); err != nil {
		t.Fatalf("tick after unfreeze: %v", err)
	}
	if m.applyCount.Load() == 0 {
		t.Fatal("unfrozen tick did not apply")
	}
}

// Deletion diff (T22/T23): when a desired-state key vanishes, the
// previously-applied resource is deleted on the node (Deleter) — a
// retired block replica's unit must be stopped, not orphaned.
func TestDeletedDesiredStateRemovesResource(t *testing.T) {
	r, _ := newDepReconciler(t, map[string][]string{"a": {}})
	ctx := context.Background()
	putDesired(t, r.store, "n1", "a", "")
	if err := r.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	// Remove the desired key; the next tick must undo the resource.
	e, err := r.store.Get(ctx, r.DesiredPrefix()+store.Key("a"))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if err := r.store.Delete(ctx, e.Key, e.Revision); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := r.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	// depManager embeds fakeManager; Deleter must have been invoked.
	r.mu.Lock()
	left := len(r.applied)
	r.mu.Unlock()
	if left != 0 {
		t.Fatalf("applied = %d entries after deletion, want 0", left)
	}
}

// flakyStore fails List on demand, as a follower's linearizable read does during an election.
type flakyStore struct {
	store.Store
	failList atomic.Bool
}

func (s *flakyStore) List(ctx context.Context, prefix store.Key) ([]*store.Entry, error) {
	if s.failList.Load() {
		return nil, fmt.Errorf("unavailable: linear read rpc failed")
	}
	return s.Store.List(ctx, prefix)
}

// deletingManager counts the resources the reconciler tears down.
type deletingManager struct {
	fakeManager
	deletes atomic.Int64
}

func (m *deletingManager) Delete(ctx context.Context, r Resource) error {
	m.deletes.Add(1)
	return nil
}

func newDeletingReconciler(t *testing.T) (*Reconciler, *flakyStore, *deletingManager) {
	s := &flakyStore{Store: testStore(t)}
	m := &deletingManager{fakeManager: fakeManager{converged: map[string]bool{}}}
	r := New(s, Options{NodeID: "n1", Logger: testLogger(), Period: time.Hour})
	r.Register(m)
	return r, s, m
}

func TestFailedDesiredStateReadTearsNothingDown(t *testing.T) {
	r, s, m := newDeletingReconciler(t)
	ctx := context.Background()
	putDesired(t, s, "n1", "vm", "")
	if err := r.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	s.failList.Store(true)
	if err := r.Tick(ctx); err == nil {
		t.Error("a tick that could not read desired state reported success")
	}
	if n := m.deletes.Load(); n != 0 {
		t.Fatalf("an unreadable desired state deleted %d running resources", n)
	}
	s.failList.Store(false)
	if err := r.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if n := m.deletes.Load(); n != 0 {
		t.Fatalf("recovering from the failed read deleted %d resources", n)
	}
}

func TestUndecodableDesiredEntryKeepsItsResource(t *testing.T) {
	r, s, m := newDeletingReconciler(t)
	ctx := context.Background()
	putDesired(t, s, "n1", "vm", "")
	if err := r.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, store.Key("/node/n1/resources/vm"), []byte("garbled")); err != nil {
		t.Fatal(err)
	}
	_ = r.Tick(ctx)
	if n := m.deletes.Load(); n != 0 {
		t.Fatalf("an undecodable entry deleted its running resource (%d deletes)", n)
	}
}

// readyManager reports every resource converged and healthy, with a fixed readiness.
type readyManager struct {
	fakeManager
	ready *bool
}

func (m *readyManager) Observe(ctx context.Context, r Resource) (Observed, error) {
	return Observed{Exists: true, InSync: true, Health: HealthHealthy, Ready: m.ready}, nil
}

func tickWithReadiness(t *testing.T, ready *bool) (resource, node string) {
	t.Helper()
	s := testStore(t)
	r := New(s, Options{NodeID: "n1", Logger: testLogger(), Period: time.Hour})
	r.Register(&readyManager{fakeManager: fakeManager{converged: map[string]bool{}}, ready: ready})
	putDesired(t, s, "n1", "vm", "")
	if err := r.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	e, err := s.Get(context.Background(), r.StatusPrefix()+"vm")
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.Get(context.Background(), "/nodes/n1/status")
	if err != nil {
		t.Fatal(err)
	}
	return string(e.Value), string(n.Value)
}

// A workload that is not ready yet (a booting VM guest) is not a node health problem.
func TestNotReadyIsRecordedWithoutDegradingTheNode(t *testing.T) {
	notReady := false
	res, node := tickWithReadiness(t, &notReady)
	if !contains(res, "health=healthy") || !contains(res, "ready=false") {
		t.Errorf("resource status = %q, want healthy and ready=false", res)
	}
	if !contains(node, "health=healthy") {
		t.Errorf("node status = %q, want healthy", node)
	}
}

func TestReadinessIsOmittedWhenNotReported(t *testing.T) {
	res, _ := tickWithReadiness(t, nil)
	if contains(res, "ready=") {
		t.Errorf("resource status = %q, want no ready field", res)
	}
	yes := true
	if res, _ := tickWithReadiness(t, &yes); !contains(res, " ready=true") {
		t.Errorf("resource status = %q, want ready=true", res)
	}
}
