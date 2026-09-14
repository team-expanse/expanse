package controller

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/scheduler"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
)

// fakeClock is the deterministic clock for the §5.2 seams: Sleep advances
// Now instead of blocking (the T12 lesson — drive state machines through
// synchronous seams, never wall-clock).
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Sleep(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// updateHarness wires a Controller with scripted §5.2 hooks.
type updateHarness struct {
	clk   *fakeClock
	start func(p *pb.PlacementStatus) error
	stop  func(p *pb.PlacementStatus) error
	ready func(p *pb.PlacementStatus) bool

	mu      sync.Mutex
	starts  []int64 // generations passed to Start
	stops   int
	stopsAt []int32 // replica indexes stopped
}

func (h *updateHarness) hooks() *UpdateHooks {
	return &UpdateHooks{
		Ready: func(_ context.Context, _ *pb.Block, p *pb.PlacementStatus) bool {
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.ready == nil {
				return true
			}
			return h.ready(p)
		},
		Start: func(_ context.Context, _ *pb.Block, p *pb.PlacementStatus) error {
			h.mu.Lock()
			h.starts = append(h.starts, p.GetGeneration())
			h.mu.Unlock()
			if h.start != nil {
				return h.start(p)
			}
			return nil
		},
		Stop: func(_ context.Context, _ *pb.Block, p *pb.PlacementStatus) error {
			h.mu.Lock()
			h.stops++
			h.stopsAt = append(h.stopsAt, p.GetReplicaIndex())
			h.mu.Unlock()
			if h.stop != nil {
				return h.stop(p)
			}
			return nil
		},
		Now:   h.clk.Now,
		Sleep: h.clk.Sleep,
	}
}

func (h *updateHarness) startCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.starts)
}

// rollBlock creates a block, places it, then updates its spec (new store
// revision = §5.2 target generation) and returns the controller plus the
// target revision.
func rollBlock(t *testing.T, ctx context.Context, b *pb.Block, nodes int) (*Controller, *updateHarness, int64) {
	t.Helper()
	st := newStore(t)
	mustCreate(t, ctx, st, b)
	h := &updateHarness{clk: newFakeClock()}
	c := New(st, func(context.Context) ([]scheduler.NodeView, scheduler.OvercommitConfig, error) {
		return nodeViews(nodes), testCfg(), nil
	})
	c.Update = h.hooks()
	if n, err := c.Reconcile(ctx); err != nil || n == 0 {
		t.Fatalf("initial Reconcile placed %d (err %v)", n, err)
	}
	waitPlaced(t, ctx, c, b.GetMetadata().GetNamespace(), b.GetMetadata().GetName(), int(b.GetSpec().GetReplicas()))

	// Spec update: bump version to change the serialized block → new
	// revision.
	b.Spec.Version = "v2"
	out, err := proto.Marshal(b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	e, err := st.Get(ctx, blockKey(b.GetMetadata().GetNamespace(), b.GetMetadata().GetName()))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, err := st.Txn(ctx, []store.Op{
		{Kind: store.OpCheck, Key: e.Key, Expect: e.Revision},
		{Kind: store.OpPut, Key: e.Key, Value: out},
	}); err != nil {
		t.Fatalf("put updated spec: %v", err)
	}
	// Revisions are store-global (status writes consume them too), so
	// re-read the block entry for its actual new revision.
	e2, err := st.Get(ctx, blockKey(b.GetMetadata().GetNamespace(), b.GetMetadata().GetName()))
	if err != nil {
		t.Fatalf("re-get block: %v", err)
	}
	return c, h, int64(e2.Revision)
}

// runRoll drives Reconcile until the block completes the roll (or fails
// fast), returning the final status and the minimum ready count observed
// across passes.
func runRoll(t *testing.T, ctx context.Context, c *Controller, h *updateHarness, ns, name string,
	want int, ready func() int,
) (*pb.BlockStatus, int) {
	t.Helper()
	minReady := want
	var status *pb.BlockStatus
	for i := 0; i < 500; i++ {
		if _, err := c.Reconcile(ctx); err != nil {
			t.Fatalf("Reconcile pass %d: %v", i, err)
		}
		status = loadStatus(t, ctx, c.St, ns, name)
		if r := ready(); r < minReady {
			minReady = r
		}
		if status.GetPhase() == pb.Phase_RUNNING || status.GetPhase() == pb.Phase_DEGRADED {
			done := true
			for _, p := range status.GetPlacements() {
				if p.GetPhase() == pb.Phase_DRAINING || p.GetPhase() == pb.Phase_STARTING {
					done = false
				}
			}
			if done {
				return status, minReady
			}
		}
	}
	t.Fatalf("roll never settled; status: %+v", status)
	return nil, 0
}

// §5.2 continuous availability (unit level): with maxUnavailable=1 and a
// replica that takes several polls to become ready, the number of ready
// replicas never drops below N-1 during the roll.
func TestRollingUpdateAvailabilityNeverBelowMaxUnavailable(t *testing.T) {
	ctx := context.Background()
	b := blockFor("web", 3)
	b.Spec.Strategy = &pb.Strategy{Update: &pb.UpdateStrategy{MaxUnavailable: 1}}
	c, h, _ := rollBlock(t, ctx, b, 3)
	ns, name := "default", "web"

	// Slow readiness: after each Stop, the restarted replica needs two
	// poll sleeps before serving again.
	h.stop = func(p *pb.PlacementStatus) error { return nil }
	readyMap := map[int32]bool{0: true, 1: true, 2: true}
	h.ready = func(p *pb.PlacementStatus) bool { return readyMap[p.GetReplicaIndex()] }
	hooks := h.hooks()
	hooks.Sleep = func(d time.Duration) {
		h.clk.Sleep(d)
		h.mu.Lock()
		for k := range readyMap {
			readyMap[k] = true
		}
		h.mu.Unlock()
	}
	hookStop := hooks.Stop
	hooks.Stop = func(ctx context.Context, blk *pb.Block, p *pb.PlacementStatus) error {
		h.mu.Lock()
		readyMap[p.GetReplicaIndex()] = false
		h.mu.Unlock()
		return hookStop(ctx, blk, p)
	}
	// readiness warms up after two poll sleeps: track sleeps since last stop
	sleepsSinceStop := 0
	oldSleep := hooks.Sleep
	hooks.Sleep = func(d time.Duration) {
		oldSleep(d)
		sleepsSinceStop++
		if sleepsSinceStop >= 2 {
			h.mu.Lock()
			for k := range readyMap {
				readyMap[k] = true
			}
			h.mu.Unlock()
		}
	}
	hookStart := hooks.Start
	hooks.Start = func(ctx context.Context, blk *pb.Block, p *pb.PlacementStatus) error {
		sleepsSinceStop = 0
		h.mu.Lock()
		readyMap[p.GetReplicaIndex()] = false
		h.mu.Unlock()
		return hookStart(ctx, blk, p)
	}
	c.Update = hooks

	readyCount := func() int {
		s := loadStatus(t, ctx, c.St, ns, name)
		n := 0
		for _, p := range s.GetPlacements() {
			if h.ready(p) && p.GetPhase() != pb.Phase_DRAINING {
				n++
			}
		}
		return n
	}
	status, minReady := runRoll(t, ctx, c, h, ns, name, 3, readyCount)
	if status.GetPhase() != pb.Phase_RUNNING {
		t.Fatalf("final phase = %v, want RUNNING", status.GetPhase())
	}
	if minReady < 2 {
		t.Errorf("ready count dipped to %d, want >= N - maxUnavailable = 2", minReady)
	}
	if len(status.GetPlacements()) != 3 {
		t.Errorf("placements = %d, want 3", len(status.GetPlacements()))
	}
	if h.stops != 3 {
		t.Errorf("stopped %d replicas, want 3 (each old replica rolled once)", h.stops)
	}
}

// §5.2 maxSurge: a surge replica is created (and becomes ready) before
// any old replica is stopped.
func TestRollingUpdateSurgeCreatesBeforeRemoving(t *testing.T) {
	ctx := context.Background()
	b := blockFor("web", 3)
	b.Spec.Strategy = &pb.Strategy{Update: &pb.UpdateStrategy{MaxUnavailable: 1, MaxSurge: 1}}
	c, h, _ := rollBlock(t, ctx, b, 4) // 4 nodes: room for the surge replica

	ns, name := "default", "web"
	status := loadStatus(t, ctx, c.St, ns, name)
	if got := len(status.GetPlacements()); got != 3 {
		t.Fatalf("pre-roll placements = %d", got)
	}
	if _, err := c.Reconcile(ctx); err != nil { // pass 1: phase → UPDATING
		t.Fatal(err)
	}
	if _, err := c.Reconcile(ctx); err != nil { // pass 2: surge create
		t.Fatal(err)
	}
	status = loadStatus(t, ctx, c.St, ns, name)
	if got := len(status.GetPlacements()); got != 4 {
		t.Fatalf("after surge pass placements = %d, want 4", got)
	}
	if h.stops != 0 {
		t.Errorf("surge pass stopped %d replicas, want 0 (create-before-remove)", h.stops)
	}
	// Finish the roll.
	final, _ := runRoll(t, ctx, c, h, ns, name, 3, func() int { return 3 })
	if final.GetPhase() != pb.Phase_RUNNING {
		t.Fatalf("final phase = %v, want RUNNING", final.GetPhase())
	}
	if got := len(final.GetPlacements()); got != 3 {
		t.Errorf("final placements = %d, want 3 (surge cleaned up)", got)
	}
	if h.stops < 4 { // 3 old replicas rolled + 1 surge replica cleaned up
		t.Errorf("total stops = %d, want >= 4", h.stops)
	}
}

// §5.2 failure handling: a start failure marks the block Degraded, aborts
// the roll, and rolls back only when autoRollback is set.
func TestRollingUpdateFailureDegradedAndRollback(t *testing.T) {
	ctx := context.Background()
	b := blockFor("web", 2)
	b.Spec.Strategy = &pb.Strategy{Update: &pb.UpdateStrategy{
		MaxUnavailable: 1, AutoRollback: true,
	}}
	c, h, target := rollBlock(t, ctx, b, 3)
	ns, name := "default", "web"

	// Fail the first new-generation start.
	h.start = func(p *pb.PlacementStatus) error {
		if p.GetGeneration() == target {
			return errStartFailed
		}
		return nil
	}
	status, _ := runRoll(t, ctx, c, h, ns, name, 2, func() int { return 2 })
	if status.GetPhase() != pb.Phase_DEGRADED {
		t.Fatalf("phase = %v, want DEGRADED", status.GetPhase())
	}
	cond := findCondition(status, abortCondition)
	if cond == nil || cond.GetReason() != strconv.FormatInt(target, 10) {
		t.Errorf("abort condition = %+v, want reason %d", cond, target)
	}
	// Rollback: the failed replica was restarted at its previous
	// generation. Two hook starts total: the failed new-gen start and the
	// rollback start (initial placement goes through the scheduler, not
	// the start hook).
	if h.startCount() < 2 {
		t.Errorf("starts = %d, want >= 2 (failed roll + rollback)", h.startCount())
	}
	if last := h.starts[len(h.starts)-1]; last == target {
		t.Errorf("last start at generation %d, want rolled-back old generation", last)
	}

	// Aborted roll does not retry while the spec is unchanged.
	starts := h.startCount()
	if _, err := c.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if h.startCount() != starts {
		t.Errorf("aborted roll retried: starts %d -> %d", starts, h.startCount())
	}
}

// Same failure without autoRollback: Degraded, no rollback start.
func TestRollingUpdateFailureNoRollback(t *testing.T) {
	ctx := context.Background()
	b := blockFor("web", 2)
	b.Spec.Strategy = &pb.Strategy{Update: &pb.UpdateStrategy{MaxUnavailable: 1}}
	c, h, target := rollBlock(t, ctx, b, 3)

	h.start = func(p *pb.PlacementStatus) error {
		if p.GetGeneration() == target {
			return errStartFailed
		}
		return nil
	}
	status, _ := runRoll(t, ctx, c, h, "default", "web", 2, func() int { return 2 })
	if status.GetPhase() != pb.Phase_DEGRADED {
		t.Fatalf("phase = %v, want DEGRADED", status.GetPhase())
	}
	for _, g := range h.starts[1:] { // after the failed roll: no old-gen re-starts
		if g != target {
			t.Errorf("unexpected non-rollback start at generation %d without autoRollback", g)
			break
		}
	}
}

var errStartFailed = errors.New("start failed")
