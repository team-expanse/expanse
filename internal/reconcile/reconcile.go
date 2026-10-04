// Package reconcile implements the node agent's reconciliation loop:
// it reads desired state from the store, builds a resource dependency DAG,
// and drives registered resource managers to converge actual state.
package reconcile

import (
	"container/list"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"math/rand"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
)

// Health is the health of an observed resource.
type Health string

const (
	HealthUnknown   Health = "unknown"
	HealthHealthy   Health = "healthy"
	HealthDegraded  Health = "degraded"
	HealthUnhealthy Health = "unhealthy"
)

// Worst returns the worse of two health values.
func Worst(a, b Health) Health {
	rank := map[Health]int{HealthHealthy: 0, HealthUnknown: 1, HealthDegraded: 2, HealthUnhealthy: 3}
	if rank[a] >= rank[b] {
		return a
	}
	return b
}

// Resource is a desired-state object the reconciler converges.
type Resource interface {
	// ID is a stable identifier, e.g. "file:/etc/motd".
	ID() string
	// Type selects the Manager, e.g. "file", "systemd-unit", "nix-config".
	Type() string
	// Dependencies lists resource IDs that must converge first.
	Dependencies() []string
}

// Observed is the actual system state for a resource.
type Observed struct {
	Exists  bool
	InSync  bool
	Details map[string]string
	Health  Health
	// Ready is whether the workload itself is serving (a VM guest booted); nil when not reported.
	// It is kept apart from Health so a workload still starting never degrades its node.
	Ready *bool
}

// Action is one step a Manager wants to take to converge a resource.
// Apply must be idempotent.
type Action struct {
	ResourceID  string
	Kind        string // "create", "update", "delete", "restart"
	Description string // human-readable, shown in UI/CLI
	Destructive bool
	Fn          func(ctx context.Context) error
}

// Manager converges resources of one Type.
type Manager interface {
	// Type selects which desired-state entries this manager handles.
	Type() string
	// Load decodes a desired-state spec into a Resource.
	Load(id string, spec []byte) (Resource, error)
	// Observe reads actual system state for the resource.
	Observe(ctx context.Context, r Resource) (Observed, error)
	// Plan compares desired vs observed and returns ordered actions
	// (possibly empty when already in sync).
	Plan(ctx context.Context, r Resource, o Observed) ([]Action, error)
	// Apply executes one action. Must be idempotent.
	Apply(ctx context.Context, a Action) error
}

// Deleter is an optional Manager capability: removing a resource's
// desired state removes the managed object from the system (e.g. delete
// the file). Managers that cannot safely undo return an error.
type Deleter interface {
	Delete(ctx context.Context, r Resource) error
}

// Options configures the reconciler loop.
type Options struct {
	NodeID         string
	Period         time.Duration // base tick interval (default 30s)
	Debounce       time.Duration // watch-trigger debounce (default 500ms)
	ObserveTimeout time.Duration // per-resource observe timeout (default 30s)
	ApplyTimeout   time.Duration // per-action apply timeout (default 120s)
	TickDeadline   time.Duration // per-tick deadline (default 5min)
	Parallelism    int           // max concurrent independent resources (default min(GOMAXPROCS,8))
	BackoffCap     time.Duration // backoff ceiling (default 30s)
	DryRun         bool
	Logger         *slog.Logger
}

func (o *Options) fill() {
	if o.Period == 0 {
		o.Period = 30 * time.Second
	}
	if o.Debounce == 0 {
		o.Debounce = 500 * time.Millisecond
	}
	if o.ObserveTimeout == 0 {
		o.ObserveTimeout = 30 * time.Second
	}
	if o.ApplyTimeout == 0 {
		o.ApplyTimeout = 120 * time.Second
	}
	if o.TickDeadline == 0 {
		o.TickDeadline = 5 * time.Minute
	}
	if o.Parallelism <= 0 {
		o.Parallelism = 8
	}
	if o.BackoffCap == 0 {
		o.BackoffCap = 30 * time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
}

// Status is the per-resource outcome of a tick.
type Status struct {
	ResourceID string
	Health     Health
	InSync     bool
	Ready      *bool
	Error      string
	Actions    int
	UpdatedAt  time.Time
}

// Metrics is a snapshot of loop counters.
type Metrics struct {
	Ticks          int64
	ChangesApplied int64
	Failures       int64
	ResourcesTotal int64
	LastTickAt     time.Time
	LastTickTookMs float64
	TickP99Ms      float64
}

// Reconciler drives the reconciliation loop.
type Reconciler struct {
	opts     Options
	store    store.Store
	logger   *slog.Logger
	managers map[string]Manager

	mu       sync.Mutex // guards resState, tickDurations, applied
	resState map[string]*resBackoff

	// applied remembers the last Resource seen per ID so that a
	// deleted desired-state key can be undone on the node (§4.4/T22:
	// a retired block replica's systemd unit must be stopped — the
	// spec vanishing is the deletion signal).
	applied map[string]Resource

	ticks          atomic.Int64
	changes        atomic.Int64
	failures       atomic.Int64
	resourcesTotal atomic.Int64
	lastTickAt     atomic.Int64 // unix nano
	lastTickTook   atomic.Int64 // duration ns
	tickDurations  []float64    // ms, guarded by mu

	trigger chan struct{} // watch-triggered tick requests
	runOnce sync.Once
	stop    chan struct{}
	stopped chan struct{}

	frozen  atomic.Bool // reconcile freeze (§4.10.4)
	retired atomic.Bool // the node left the cluster; nothing is applied again
	tickMu  sync.Mutex  // one Tick or Retire at a time
}

type resBackoff struct {
	failures    int
	nextAttempt time.Time
}

// New creates a Reconciler.
func New(s store.Store, opts Options) *Reconciler {
	opts.fill()
	return &Reconciler{
		opts:     opts,
		store:    s,
		logger:   opts.Logger.With("component", "reconcile"),
		managers: map[string]Manager{},
		resState: map[string]*resBackoff{},
		applied:  map[string]Resource{},
		trigger:  make(chan struct{}, 1),
		stop:     make(chan struct{}),
		stopped:  make(chan struct{}),
	}
}

// Register adds a resource manager.
func (r *Reconciler) Register(m Manager) {
	r.managers[m.Type()] = m
}

// DesiredPrefix is the store prefix holding this node's desired state.
func (r *Reconciler) DesiredPrefix() store.Key {
	return store.Key(fmt.Sprintf("/node/%s/resources/", r.opts.NodeID))
}

// StatusPrefix is the store prefix holding per-resource observed status.
func (r *Reconciler) StatusPrefix() store.Key {
	return store.Key(fmt.Sprintf("/node/%s/status/resources/", r.opts.NodeID))
}

// ManagerFor returns the registered manager for a resource type (or nil).
func (r *Reconciler) ManagerFor(typ string) Manager {
	return r.managers[typ]
}

// Trigger requests an immediate (debounced) tick, e.g. on a store watch event.
func (r *Reconciler) Trigger() {
	select {
	case r.trigger <- struct{}{}:
	default: // already pending
	}
}

// SetDryRun toggles dry-run mode (used by the Reconcile API).
func (r *Reconciler) SetDryRun(on bool) {
	r.mu.Lock()
	r.opts.DryRun = on
	r.mu.Unlock()
}

// Freeze toggles the reconcile freeze (§4.10.4): a frozen reconciler
// stops applying new desired state but existing workloads keep running
// (they are untouched on disk either way). Used when the node cannot
// reach quorum: availability over consistency for what already runs,
// consistency for changes.
func (r *Reconciler) Freeze(on bool) {
	if r.retired.Load() {
		return
	}
	if r.frozen.Swap(on) != on {
		r.logger.Info("reconcile freeze toggled", "frozen", on)
	}
}

// Frozen reports the current freeze state.
func (r *Reconciler) Frozen() bool { return r.frozen.Load() }

// Retire freezes the reconciler for good and deletes every resource it applied or
// the local store copy desires, dependents first: the node has left the cluster.
func (r *Reconciler) Retire(ctx context.Context) {
	r.retired.Store(true)
	r.frozen.Store(true)
	r.tickMu.Lock()
	defer r.tickMu.Unlock()
	r.mu.Lock()
	all := maps.Clone(r.applied)
	clear(r.applied)
	r.mu.Unlock()
	if local, _, err := r.loadDesired(store.WithStale(ctx)); err == nil {
		maps.Copy(all, local)
	} else {
		r.logger.Warn("retire: local desired state unreadable", "err", err)
	}
	order, err := topoSort(all)
	if err != nil {
		order = slices.Collect(maps.Keys(all))
	}
	slices.Reverse(order)
	for _, id := range order {
		res := all[id]
		d, ok := r.managers[res.Type()].(Deleter)
		if !ok {
			continue
		}
		if err := d.Delete(ctx, res); err != nil {
			r.logger.Warn("retire: delete failed", "id", id, "err", err)
		} else {
			r.logger.Info("retire: removed resource", "id", id, "type", res.Type())
		}
	}
}

// Run runs the loop until ctx is canceled: periodic ticks, debounced
// watch-triggered ticks, and one immediate tick at startup.
func (r *Reconciler) Run(ctx context.Context) {
	defer close(r.stopped)
	// Register the watch BEFORE the startup tick so there is no window in
	// which desired-state writes go unnoticed (watches have no replay).
	r.runOnce.Do(func() {
		go r.watchDesired(ctx)
	})

	// Immediate first tick so the node converges quickly at startup (and
	// covers any writes that happened before the watch was registered).
	if err := r.Tick(ctx); err != nil {
		r.logger.Error("startup tick failed", "err", err)
	}

	ticker := time.NewTicker(r.opts.Period)
	defer ticker.Stop()
	var debounce *time.Timer
	var debounceC <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stop:
			return
		case <-ticker.C:
			if err := r.Tick(ctx); err != nil {
				r.logger.Error("tick failed", "err", err)
			}
		case <-r.trigger:
			if debounce != nil {
				debounce.Reset(r.opts.Debounce)
			} else {
				debounce = time.NewTimer(r.opts.Debounce)
				debounceC = debounce.C
			}
		case <-debounceC:
			debounce = nil
			debounceC = nil
			if err := r.Tick(ctx); err != nil {
				r.logger.Error("tick failed", "err", err)
			}
		}
	}
}

// Stop halts the loop (used by Shutdown API).
func (r *Reconciler) Stop() {
	select {
	case <-r.stop:
		return
	default:
		close(r.stop)
	}
	<-r.stopped
}

// watchDesired triggers debounced ticks on any desired-state change.
func (r *Reconciler) watchDesired(ctx context.Context) {
	ch, err := r.store.Watch(ctx, r.DesiredPrefix(), 0)
	if err != nil {
		r.logger.Error("watch desired state failed", "err", err)
		return
	}
	for range ch {
		r.Trigger()
	}
}

// Tick runs one reconciliation round. It is safe to call concurrently with
// the loop; only one tick runs at a time (a coarse loop lock serializes).
func (r *Reconciler) Tick(ctx context.Context) error {
	r.tickMu.Lock()
	defer r.tickMu.Unlock()
	// §4.10.4 reconcile freeze: skip the round entirely — no desired
	// state is applied, no status is written (status writes would fail
	// anyway on a non-quorum node).
	if r.frozen.Load() {
		return nil
	}
	tctx, cancel := context.WithTimeout(ctx, r.opts.TickDeadline)
	defer cancel()

	start := time.Now()
	r.ticks.Add(1)

	resources, errs, err := r.loadDesired(tctx)
	if err != nil {
		// Unreadable is not empty: a leader election must not tear down running workloads.
		r.failures.Add(1)
		return fmt.Errorf("desired state unreadable, tick skipped: %w", err)
	}
	for id, err := range errs {
		r.logger.Error("invalid desired state", "id", id, "err", err)
		r.recordStatus(tctx, &Status{ResourceID: id, Health: HealthUnhealthy, Error: err.Error(), UpdatedAt: time.Now()})
	}
	r.resourcesTotal.Store(int64(len(resources)))

	// Deletion diff: IDs applied on a previous tick but absent from
	// desired state now were deleted — undo them via the manager.
	r.mu.Lock()
	for id, old := range r.applied {
		if _, undecodable := errs[id]; undecodable {
			continue // a garbled entry keeps its resource until the entry is fixed or deleted
		}
		if _, ok := resources[id]; !ok {
			delete(r.applied, id)
			r.mu.Unlock()
			if m := r.managers[old.Type()]; m != nil {
				if d, ok := m.(Deleter); ok {
					if err := d.Delete(ctx, old); err != nil {
						r.logger.Warn("orphan cleanup failed", "id", id, "err", err)
					} else {
						r.changes.Add(1)
						r.logger.Info("removed undesired resource", "id", id, "type", old.Type())
					}
				}
			}
			r.mu.Lock()
		}
	}
	for id, res := range resources {
		r.applied[id] = res
	}
	r.mu.Unlock()

	order, err := topoSort(resources)
	if err != nil {
		// Cycle: abort tick, emit error event (status write).
		r.failures.Add(1)
		r.recordNodeStatus(tctx, fmt.Sprintf("dependency cycle: %v", err))
		return fmt.Errorf("dependency cycle: %w", err)
	}

	// Process in dependency order; resources whose dependencies are all
	// done may run in parallel, bounded by Parallelism.
	var (
		wg       sync.WaitGroup
		sem      = make(chan struct{}, r.opts.Parallelism)
		statusCh = make(chan *Status, len(order))
		aggMu    sync.Mutex
		agg      = HealthHealthy
		changes  int64
		failures int64
	)
	// Kahn levels: each level's resources are mutually independent.
	levels := dependencyLevels(order, resources)
	for _, level := range levels {
		for _, id := range level {
			res := resources[id]
			wg.Add(1)
			go func(res Resource) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				st := r.processResource(tctx, res)
				statusCh <- st
				aggMu.Lock()
				agg = Worst(agg, st.Health)
				if st.Actions > 0 && st.Error == "" {
					changes++
				}
				if st.Error != "" {
					failures++
				}
				aggMu.Unlock()
			}(res)
		}
		wg.Wait() // next level only after this level fully converges
	}
	close(statusCh)
	for st := range statusCh {
		r.recordStatus(tctx, st)
	}

	r.changes.Add(changes)
	r.failures.Add(failures)
	took := time.Since(start)
	r.lastTickAt.Store(start.UnixNano())
	r.lastTickTook.Store(int64(took))
	r.recordTickDuration(float64(took.Milliseconds()))
	r.recordNodeStatus(tctx, string(agg))
	r.logger.Info("tick complete",
		"resources", len(order), "changes", changes, "failures", failures,
		"took", took.Round(time.Millisecond))
	return nil
}

// processResource observes, plans, and applies one resource.
func (r *Reconciler) processResource(ctx context.Context, res Resource) *Status {
	st := &Status{ResourceID: res.ID(), Health: HealthUnknown, UpdatedAt: time.Now()}
	m, ok := r.managers[res.Type()]
	if !ok {
		st.Health = HealthUnhealthy
		st.Error = fmt.Sprintf("no manager registered for type %q", res.Type())
		return st
	}

	// Backoff: skip resources that recently failed and are not yet due.
	if delay, due := r.backoffState(res.ID()); !due {
		st.Health = HealthDegraded
		st.UpdatedAt = time.Now()
		r.logger.Warn("resource in backoff, deferred", "id", res.ID(), "retry_in", delay.Round(time.Millisecond))
		return st
	}

	obs, err := r.observe(ctx, m, res)
	if err != nil {
		r.markFailure(res.ID())
		st.Health = HealthUnhealthy
		st.Error = fmt.Sprintf("observe: %v", err)
		return st
	}
	if obs.InSync {
		r.markSuccess(res.ID())
		st.Health, st.Ready = obs.Health, obs.Ready
		st.InSync = true
		return st
	}

	actions, err := m.Plan(ctx, res, obs)
	if err != nil {
		r.markFailure(res.ID())
		st.Health = HealthUnhealthy
		st.Error = fmt.Sprintf("plan: %v", err)
		return st
	}
	if len(actions) == 0 {
		// Not in sync but nothing to do (manager waiting on external state).
		r.markSuccess(res.ID())
		st.Health, st.Ready = obs.Health, obs.Ready
		st.InSync = true
		return st
	}

	st.Actions = len(actions)
	if r.opts.DryRun {
		st.Health = HealthDegraded
		r.logger.Info("dry-run: would apply", "id", res.ID(), "actions", len(actions))
		return st
	}

	for _, a := range actions {
		actx, cancel := context.WithTimeout(ctx, r.opts.ApplyTimeout)
		r.logger.Info("applying action",
			"id", res.ID(), "kind", a.Kind, "action", a.Description, "destructive", a.Destructive)
		err := m.Apply(actx, a)
		cancel()
		if err != nil {
			r.markFailure(res.ID())
			st.Health = HealthUnhealthy
			st.Error = fmt.Sprintf("apply %s: %v", a.Kind, err)
			return st
		}
	}

	// Re-observe to verify convergence.
	obs2, err := r.observe(ctx, m, res)
	if err != nil {
		st.Health = HealthDegraded
		st.Error = fmt.Sprintf("re-observe: %v", err)
		return st
	}
	if !obs2.InSync {
		r.markFailure(res.ID())
		st.Health = Worst(obs2.Health, HealthDegraded)
		st.Error = "still out of sync after apply"
		return st
	}
	r.markSuccess(res.ID())
	st.Health, st.Ready = obs2.Health, obs2.Ready
	st.InSync = true
	return st
}

// observe runs the manager's Observe with a per-resource timeout.
func (r *Reconciler) observe(ctx context.Context, m Manager, res Resource) (Observed, error) {
	octx, cancel := context.WithTimeout(ctx, r.opts.ObserveTimeout)
	defer cancel()
	return m.Observe(octx, res)
}

// ---- Backoff ----

// BackoffBase returns the base backoff for the n-th consecutive failure
// (n >= 1): 1s, 2s, 4s, 8s, 16s, 30s, 30s, ... (capped at cap).
func BackoffBase(n int, cap time.Duration) time.Duration {
	if n < 1 {
		return 0
	}
	d := time.Duration(1) << uint(n-1) * time.Second
	if d > cap {
		return cap
	}
	return d
}

// jitter applies ±20% jitter to d.
func jitter(d time.Duration, rng *rand.Rand) time.Duration {
	if d <= 0 {
		return 0
	}
	f := 0.8 + 0.4*rng.Float64()
	return time.Duration(float64(d) * f)
}

func (r *Reconciler) backoffState(id string) (time.Duration, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rs, ok := r.resState[id]
	if !ok || rs.failures == 0 {
		return 0, true
	}
	now := time.Now()
	if now.Before(rs.nextAttempt) {
		return rs.nextAttempt.Sub(now), false
	}
	return 0, true
}

func (r *Reconciler) markFailure(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rs := r.resState[id]
	if rs == nil {
		rs = &resBackoff{}
		r.resState[id] = rs
	}
	rs.failures++
	// ±20% jitter so a fleet of failing nodes doesn't thundering-herd.
	rs.nextAttempt = time.Now().Add(jitter(BackoffBase(rs.failures, r.opts.BackoffCap), rand.New(rand.NewSource(time.Now().UnixNano()))))
}

func (r *Reconciler) markSuccess(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.resState, id) // success resets backoff
}

// ---- Status & metrics ----

func (r *Reconciler) recordStatus(ctx context.Context, st *Status) {
	key := r.StatusPrefix() + store.Key(st.ResourceID)
	val := fmt.Sprintf("health=%s in_sync=%t", st.Health, st.InSync)
	if st.Ready != nil {
		val += fmt.Sprintf(" ready=%t", *st.Ready) // before the free-text error, so parsers never confuse the two
	}
	val += fmt.Sprintf(" actions=%d error=%q updated=%d", st.Actions, st.Error, st.UpdatedAt.UnixNano())
	if _, err := r.store.Put(ctx, key, []byte(val)); err != nil {
		r.logger.Error("record status failed", "id", st.ResourceID, "err", err)
	}
}

// recordNodeStatus writes the tick summary to /nodes/<id>/reconcile. /nodes/<id>/status is the agent's
// (node health checks decide placement); a failing workload must not mark its node unfit.
func (r *Reconciler) recordNodeStatus(ctx context.Context, health string) {
	key := store.Key(fmt.Sprintf("/nodes/%s/reconcile", r.opts.NodeID))
	val := fmt.Sprintf("health=%s ticks=%d changes=%d failures=%d updated=%d",
		health, r.ticks.Load(), r.changes.Load(), r.failures.Load(), time.Now().UnixNano())
	if _, err := r.store.Put(ctx, key, []byte(val)); err != nil {
		r.logger.Error("record node status failed", "err", err)
	}
}

func (r *Reconciler) recordTickDuration(ms float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tickDurations = append(r.tickDurations, ms)
	if len(r.tickDurations) > 1000 {
		r.tickDurations = r.tickDurations[len(r.tickDurations)-1000:]
	}
}

// Metrics returns a snapshot of loop counters.
func (r *Reconciler) Metrics() Metrics {
	m := Metrics{
		Ticks:          r.ticks.Load(),
		ChangesApplied: r.changes.Load(),
		Failures:       r.failures.Load(),
		ResourcesTotal: r.resourcesTotal.Load(),
	}
	if at := r.lastTickAt.Load(); at != 0 {
		m.LastTickAt = time.Unix(0, at)
		m.LastTickTookMs = float64(r.lastTickTook.Load()) / 1e6
	}
	r.mu.Lock()
	m.TickP99Ms = percentile(r.tickDurations, 0.99)
	r.mu.Unlock()
	return m
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	// Copy + sort to avoid mutating the shared slice.
	cp := make([]float64, len(sorted))
	copy(cp, sorted)
	sort.Float64s(cp)
	idx := int(p * float64(len(cp)-1))
	return cp[idx]
}

// ---- Desired state & DAG ----

// loadDesired reads and decodes all desired-state entries; err is set only when the list itself failed.
func (r *Reconciler) loadDesired(ctx context.Context) (map[string]Resource, map[string]error, error) {
	entries, err := r.store.List(ctx, r.DesiredPrefix())
	if err != nil {
		return nil, nil, fmt.Errorf("list desired: %w", err)
	}
	resources := map[string]Resource{}
	errs := map[string]error{}
	for _, e := range entries {
		id := string(e.Key)[len(r.DesiredPrefix()):]
		// The spec encodes its type on the first line: "type: <t>\n<spec>".
		typ, spec, err := SplitType(id, e.Value)
		if err != nil {
			errs[id] = err
			continue
		}
		m, ok := r.managers[typ]
		if !ok {
			errs[id] = errors.New(errors.KindInvalid, "load",
				fmt.Sprintf("no manager registered for type %q", typ))
			continue
		}
		res, err := m.Load(id, spec)
		if err != nil {
			errs[id] = err
			continue
		}
		resources[id] = res
	}
	return resources, errs, nil
}

// SplitType decodes the "type: <t>" header line from a spec.
func SplitType(id string, v []byte) (string, []byte, error) {
	line := v
	rest := []byte(nil)
	if i := indexByte(v, '\n'); i >= 0 {
		line, rest = v[:i], v[i+1:]
	}
	if len(line) < 6 || string(line[:6]) != "type: " {
		return "", nil, errors.New(errors.KindInvalid, "load",
			fmt.Sprintf("spec for %s must start with \"type: <type>\"", id))
	}
	return string(line[6:]), rest, nil
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// topoSort returns resources in dependency order (dependencies first) or an
// error describing a dependency cycle. Unknown dependencies are ignored
// (they may be failed/absent resources; the dependent still runs).
func topoSort(resources map[string]Resource) ([]string, error) {
	order, _, err := topoSortWithLevels(resources)
	return order, err
}

func topoSortWithLevels(resources map[string]Resource) ([]string, [][]string, error) {
	// Build the DAG. indegree = number of dependencies present in the set.
	indegree := map[string]int{}
	dependents := map[string][]string{} // dep -> resources that depend on it
	for id, res := range resources {
		indegree[id] = 0
		for _, dep := range res.Dependencies() {
			if _, ok := resources[dep]; ok {
				indegree[id]++
				dependents[dep] = append(dependents[dep], id)
			}
		}
	}
	// Kahn's algorithm, seeded deterministically for stable output.
	queue := list.New()
	var seeds []string
	for id, d := range indegree {
		if d == 0 {
			seeds = append(seeds, id)
		}
	}
	sort.Strings(seeds)
	for _, s := range seeds {
		queue.PushBack(s)
	}
	var order []string
	var levels [][]string
	processed := 0
	for queue.Len() > 0 {
		var level []string
		n := queue.Len()
		for i := 0; i < n; i++ {
			el := queue.Front()
			id := el.Value.(string)
			queue.Remove(el)
			order = append(order, id)
			level = append(level, id)
			processed++
			var next []string
			for _, dep := range dependents[id] {
				indegree[dep]--
				if indegree[dep] == 0 {
					next = append(next, dep)
				}
			}
			sort.Strings(next)
			for _, x := range next {
				queue.PushBack(x)
			}
		}
		levels = append(levels, level)
	}
	if processed != len(resources) {
		// Find the cycle members for a useful error.
		var stuck []string
		for id, d := range indegree {
			if d > 0 {
				stuck = append(stuck, id)
			}
		}
		sort.Strings(stuck)
		return nil, nil, fmt.Errorf("cycle among resources: %v", stuck)
	}
	return order, levels, nil
}

// dependencyLevels maps a topological order into parallel-safe levels
// (resources in the same level have no dependency relationship).
func dependencyLevels(order []string, resources map[string]Resource) [][]string {
	_, levels, err := topoSortWithLevels(resources)
	if err != nil {
		// Should not happen (order was already validated), fall back to serial.
		levels = make([][]string, len(order))
		for i, id := range order {
			levels[i] = []string{id}
		}
	}
	return levels
}
