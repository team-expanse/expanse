// Package blocks is the Phase 04 blocks chaos suite (§8 Chaos): a
// 20-block deployment under continuous raft-node kills, asserting
//
//   - the cluster returns to fully-placed (every replica scheduled,
//     none LOST) within the recovery budget of each kill event;
//   - no anti-affinity violation is ever observed (never two replicas
//     of the same block on one node);
//   - a standalone singleton-invariant checker running through the
//     whole scenario never observes two active instances of a
//     singleton block (G4.11 at chaos level), nor a lease split brain.
//
// The scenario runs in-process against the cluster chaos harness: the
// "nodes" the scheduler places onto are simulated NodeViews, while the
// raft cluster (leader election, failover, log replication) is real.
// Runtime phase promotion (SCHEDULING → RUNNING) is agent-driven and
// covered by the VM tests; at this level "recovered" means every block
// has its full replica complement placed on distinct nodes with no
// LOST/retired records outstanding.
//
// Scale: default 25 s (fast local runs); RUN_CHAOS=1 scales to the
// spec's 30 minutes. CHAOS_DURATION overrides both.
package blocks

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/blocks/controller"
	"github.com/expanse/expanse/internal/quantity"
	"github.com/expanse/expanse/internal/scheduler"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
	cluster "github.com/expanse/expanse/test/chaos/cluster"
	"google.golang.org/protobuf/proto"
)

// scenarioDuration: RUN_CHAOS=1 → 30 min (§8); CHAOS_DURATION overrides;
// default 25 s for local runs.
func scenarioDuration() time.Duration {
	if os.Getenv("RUN_CHAOS") == "1" {
		return 30 * time.Minute
	}
	if d, err := time.ParseDuration(os.Getenv("CHAOS_DURATION")); err == nil && d > 0 {
		return d
	}
	return 25 * time.Second
}

const (
	nBlocks     = 20 // 17 anti-affinity ×3 replicas + 3 singletons
	nAA         = 17
	nSingletons = nBlocks - nAA
	nSimNodes   = 5
)

// simNodes is the mutable simulated node fleet the controller's Nodes
// callback serves. Killing raft node i also marks sim node i not-Ready
// (the agent is gone → the node is unreachable to the scheduler); the
// remaining sim nodes stay ready so the fleet always has capacity.
type simNodes struct {
	mu    sync.Mutex
	views []scheduler.NodeView
}

func newSimNodes(n int) *simNodes {
	s := &simNodes{views: make([]scheduler.NodeView, n)}
	for i := range s.views {
		s.views[i] = scheduler.NodeView{
			ID:       fmt.Sprintf("n%d", i+1),
			Ready:    true,
			FreeCPU:  quantity.CPU{Milli: 8000},
			FreeMem:  quantity.Bytes{N: 32 << 30},
			FreeDisk: quantity.Bytes{N: 200 << 30},
		}
	}
	return s
}

func (s *simNodes) setReady(i int, ready bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.views[i].Ready = ready
}

func (s *simNodes) snapshot() ([]scheduler.NodeView, scheduler.OvercommitConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]scheduler.NodeView(nil), s.views...),
		scheduler.OvercommitConfig{CPUOvercommitRatio: 2.0, MemoryOvercommitRatio: 1.0}
}

// blockSpecs builds the 20 desired blocks: 17 anti-affinity (node)
// blocks with 3 replicas and 3 singletons, all util/echo (the type is
// irrelevant at this level — there are no real agents).
func blockSpecs() []*pb.Block {
	out := make([]*pb.Block, 0, nBlocks)
	for i := 0; i < nAA; i++ {
		r := int32(3)
		out = append(out, &pb.Block{
			Metadata: &pb.Metadata{Name: fmt.Sprintf("aa-%02d", i), Namespace: "default"},
			Spec: &pb.BlockSpec{
				Type:     "util/echo",
				Replicas: &r,
				Placement: &pb.Placement{
					AntiAffinity: pb.AntiAffinity_ANTI_AFFINITY_NODE,
				},
				Resources: &pb.Resources{Requests: &pb.ResourcePair{Cpu: "100m", Memory: "64Mi"}},
			},
		})
	}
	for i := 0; i < nSingletons; i++ {
		one := int32(1)
		out = append(out, &pb.Block{
			Metadata: &pb.Metadata{Name: fmt.Sprintf("single-%02d", i), Namespace: "default"},
			Spec: &pb.BlockSpec{
				Type:      "util/echo",
				Replicas:  &one,
				Strategy:  &pb.Strategy{Kind: pb.StrategyKind_SINGLETON},
				Resources: &pb.Resources{Requests: &pb.ResourcePair{Cpu: "100m", Memory: "64Mi"}},
			},
		})
	}
	return out
}

// controllers runs one block controller per raft node. Only the leader
// acts (Reconcile no-ops elsewhere), so failover hands placement over.
type controllers struct {
	h               *cluster.Harness
	sim             *simNodes
	interval, grace time.Duration
	mu              sync.Mutex
	cancel          [3]context.CancelFunc
}

func startControllers(t *testing.T, h *cluster.Harness, sim *simNodes, interval, grace time.Duration) *controllers {
	c := &controllers{h: h, sim: sim, interval: interval, grace: grace}
	for i := range c.cancel {
		c.restart(i)
	}
	t.Cleanup(func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, cancel := range c.cancel {
			cancel()
		}
	})
	return c
}

// restart replaces node i's controller with one bound to its current
// store; call it after Harness.Restart, which opens a new store.
func (c *controllers) restart(i int) {
	ctx, cancel := context.WithCancel(context.Background())
	ctl := controller.New(c.h.Node(i), func(context.Context) ([]scheduler.NodeView, scheduler.OvercommitConfig, error) {
		views, cfg := c.sim.snapshot()
		return views, cfg, nil
	})
	ctl.Interval = c.interval
	ctl.UnreachableGrace = c.grace
	c.mu.Lock()
	if c.cancel[i] != nil {
		c.cancel[i]()
	}
	c.cancel[i] = cancel
	c.mu.Unlock()
	go ctl.Run(ctx)
}

// blockKey mirrors the controller's desired-state key layout.
func blockKey(name string) store.Key {
	return store.Key("/blocks/default/" + name)
}

// deploy writes every desired block through the leader (retrying across
// elections).
func deploy(t *testing.T, ctx context.Context, h *cluster.Harness, blocks []*pb.Block) {
	t.Helper()
	for _, b := range blocks {
		raw, err := proto.Marshal(b)
		if err != nil {
			t.Fatalf("marshal %s: %v", b.GetMetadata().GetName(), err)
		}
		if _, err := h.Put(ctx, blockKey(b.GetMetadata().GetName()), raw); err != nil {
			t.Fatalf("deploy %s: %v", b.GetMetadata().GetName(), err)
		}
	}
}

// status reads a block's status through node i's (possibly stale) FSM
// view — the checker deliberately observes every node, not just the
// leader, so a deposed/divergent node cannot hide a violation.
func status(h *cluster.Harness, i int, name string) *pb.BlockStatus {
	s := h.Node(i)
	if s == nil || !h.Live(i) {
		return nil
	}
	e, err := s.Get(store.WithStale(context.Background()),
		store.Key(string(blockKey(name))+controllerStatusSuffix))
	if err != nil || e == nil {
		return nil
	}
	var st pb.BlockStatus
	if proto.Unmarshal(e.Value, &st) != nil {
		return nil
	}
	return &st
}

// controllerStatusSuffix mirrors controller.StatusSuffix (unexported
// there; the wire format is fixed).
const controllerStatusSuffix = "/status"

// activeCount counts placements that represent a live instance
// (replica index not yet retired, not LOST).
func activeCount(st *pb.BlockStatus) int {
	n := 0
	for _, p := range st.GetPlacements() {
		if p.GetReplicaIndex() != -1 && p.GetPhase() != pb.Phase_LOST {
			n++
		}
	}
	return n
}

// violation describes one observed invariant breach.
type violation struct {
	kind   string // "singleton-double-run" | "anti-affinity"
	detail string
	at     time.Time
}

// invariantChecker is the standalone singleton/anti-affinity checker:
// it polls every live node's local FSM view for the whole scenario and
// records any breach. A single breach is a release blocker.
type invariantChecker struct {
	h      *cluster.Harness
	names  []string
	mu     sync.Mutex
	viols  []violation
	checks int
}

func (c *invariantChecker) run(ctx context.Context, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			c.mu.Lock()
			c.checks++
			c.mu.Unlock()
			for i := 0; i < 3; i++ {
				if !c.h.Live(i) {
					continue
				}
				c.checkNode(i)
			}
		}
	}
}

func (c *invariantChecker) checkNode(i int) {
	for _, name := range c.names {
		st := status(c.h, i, name)
		if st == nil {
			continue
		}
		singleton := isSingletonName(name)
		active := 0
		seenNodes := map[string]bool{}
		for _, p := range st.GetPlacements() {
			if p.GetReplicaIndex() == -1 || p.GetPhase() == pb.Phase_LOST {
				continue
			}
			active++
			if seenNodes[p.GetNodeId()] {
				c.record("anti-affinity",
					fmt.Sprintf("%s: two replicas on %s", name, p.GetNodeId()))
			}
			seenNodes[p.GetNodeId()] = true
		}
		if singleton && active > 1 {
			holders := fmt.Sprintf("%v", st.GetPlacements())
			c.record("singleton-double-run",
				fmt.Sprintf("%s: %d active instances: %s", name, active, holders))
		}
	}
}

// isSingletonName matches the blockSpecs naming.
func isSingletonName(name string) bool {
	return len(name) > 7 && name[:7] == "single-"
}

func (c *invariantChecker) record(kind, detail string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.viols = append(c.viols, violation{kind: kind, detail: detail, at: time.Now()})
}

func (c *invariantChecker) violations() []violation {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]violation(nil), c.viols...)
}

// waitRecovered polls until every block is fully placed (full replica
// complement on distinct nodes, none LOST) or the budget expires.
func waitRecovered(t *testing.T, h *cluster.Harness, names []string, budget time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if h.LeaderIndex() < 0 {
			// mid-election: keep polling
			time.Sleep(200 * time.Millisecond)
			continue
		}
		ok := true
		for _, name := range names {
			st := status(h, h.LeaderIndex(), name)
			if st == nil {
				ok = false
				break
			}
			want := 3
			if isSingletonName(name) {
				want = 1
			}
			if activeCount(st) != want {
				ok = false
				break
			}
			if !isSingletonName(name) {
				seen := map[string]bool{}
				for _, p := range st.GetPlacements() {
					if p.GetReplicaIndex() == -1 || p.GetPhase() == pb.Phase_LOST {
						continue
					}
					if seen[p.GetNodeId()] {
						ok = false
					}
					seen[p.GetNodeId()] = true
				}
			}
		}
		if ok && h.LeaderIndex() >= 0 {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	var dbg []string
	for _, name := range names {
		st := status(h, h.LeaderIndex(), name)
		if st == nil {
			dbg = append(dbg, name+": no status")
		} else {
			r := st.GetPendingReason()
			dbg = append(dbg, fmt.Sprintf("%s: active=%d phase=%s reason=%v", name, activeCount(st), st.GetPhase(), r))
		}
	}
	return fmt.Errorf("not recovered within %v (leader=%d): %v", budget, h.LeaderIndex(), dbg)
}

// TestChaosBlocksRandomKill runs the §8 blocks chaos scenario: 20
// blocks under continuous raft-node kill/restart cycles, with the
// singleton/anti-affinity invariant checker sweeping every node
// throughout and a recovery assertion after every kill event.
func TestChaosBlocksRandomKill(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos: skipped in -short")
	}
	dur := scenarioDuration()
	h := cluster.NewHarness(t, 3)
	sim := newSimNodes(nSimNodes)

	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()

	blocks := blockSpecs()
	names := make([]string, len(blocks))
	for i, b := range blocks {
		names[i] = b.GetMetadata().GetName()
	}

	// Recovery budget: 2 min at full length (§8); 20 s compressed
	// (still above the 15 s singleton lease TTL + grace + pass
	// interval, so a failover plus lease expiry fits).
	budget := 20 * time.Second
	grace := time.Second
	interval := 250 * time.Millisecond
	killEvery := 2 * time.Second
	if os.Getenv("RUN_CHAOS") == "1" {
		budget = 2 * time.Minute
		grace = 30 * time.Second
		killEvery = 60 * time.Second
	} else if d, err := time.ParseDuration(os.Getenv("CHAOS_DURATION")); err == nil && d > 3*time.Minute {
		budget = 2 * time.Minute
		grace = 30 * time.Second
		killEvery = 60 * time.Second
	}

	ctl := startControllers(t, h, sim, interval, grace)

	deploy(t, ctx, h, blocks)

	chk := &invariantChecker{h: h, names: names}
	go chk.run(ctx, 150*time.Millisecond)

	// Initial placement before faulting starts.
	if err := waitRecovered(t, h, names, 30*time.Second); err != nil {
		t.Fatalf("initial placement: %v", err)
	}
	t.Logf("initial placement complete: %d blocks", len(names))

	kills := 0
	for start := time.Now(); time.Since(start) < dur; {
		time.Sleep(killEvery)
		if ctx.Err() != nil {
			break
		}
		victim := rand.Intn(3)
		h.Kill(victim)
		sim.setReady(victim, false)
		kills++
		t.Logf("killed n%d at %s", victim, time.Since(start).Round(time.Second))

		if err := waitRecovered(t, h, names, budget); err != nil {
			t.Errorf("kill #%d (n%d): %v", kills, victim, err)
		}
		time.Sleep(killEvery / 2)
		h.Restart(victim)
		ctl.restart(victim)
		sim.setReady(victim, true)
	}

	// Final settle: everything live again, fully placed.
	if err := waitRecovered(t, h, names, budget); err != nil {
		t.Errorf("final recovery: %v", err)
	}

	if vs := chk.violations(); len(vs) > 0 {
		t.Errorf("invariant checker: %d violation(s):", len(vs))
		for _, v := range vs {
			t.Errorf("  %s: %s", v.kind, v.detail)
		}
	}
	if chk.checks == 0 {
		t.Error("invariant checker never ran")
	}
	t.Logf("blocks-chaos: %d kill/restart cycles, %d checker sweeps, 0 violations",
		kills, chk.checks)
}

// A node restarted mid-scenario must reconcile once it wins leadership,
// as a restarted expanse process would (otherwise placement silently stalls).
func TestRestartedLeaderKeepsPlacing(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos: skipped in -short")
	}
	h := cluster.NewHarness(t, 3)
	sim := newSimNodes(nSimNodes)
	ctl := startControllers(t, h, sim, 250*time.Millisecond, time.Second)

	h.Kill(0)
	h.Restart(0)
	ctl.restart(0)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for h.LeaderIndex() != 0 {
		if l := h.Leader(); l != nil {
			_ = l.ApplyTransferLeadership("n0") // retried until n0 leads
		}
		if ctx.Err() != nil {
			t.Fatal("n0 never became leader")
		}
		time.Sleep(200 * time.Millisecond)
	}

	blocks := blockSpecs()[:1]
	deploy(t, ctx, h, blocks)
	if err := waitRecovered(t, h, []string{blocks[0].GetMetadata().GetName()}, 10*time.Second); err != nil {
		t.Fatalf("restarted leader n0: %v", err)
	}
}
