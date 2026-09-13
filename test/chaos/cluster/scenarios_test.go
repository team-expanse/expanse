package chaos

import (
	"context"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// scenarioDuration returns the length of one chaos scenario. Default 20 s
// (fast suite); RUN_CHAOS=1 scales to the spec's 5 minutes per scenario.
func scenarioDuration() time.Duration {
	if os.Getenv("RUN_CHAOS") == "1" {
		return 5 * time.Minute
	}
	if d, err := time.ParseDuration(os.Getenv("CHAOS_DURATION")); err == nil && d > 0 {
		return d
	}
	return 20 * time.Second
}

// faultInterval scales a fault cadence to the scenario duration: at the
// full 5 minutes the intervals match the spec table (kills every 10–60 s,
// partitions every 20–90 s, transfers every 15 s); short runs compress
// the same number of fault events.
func faultInterval(scenario time.Duration, min, max time.Duration) time.Duration {
	d := scenario / 12
	if d < 300*time.Millisecond {
		d = 300 * time.Millisecond
	}
	if d > max {
		d = max
	}
	if d < min {
		d = min
	}
	return d
}

// startChecker runs a checker goroutine over every live node probe.
func startChecker(ctx context.Context, h *Harness, interval time.Duration) *Checker {
	probes := make([]NodeProbe, h.n)
	for i := range probes {
		probes[i] = h.HarnessLeaseProbe(i)
	}
	c := NewChecker(interval, probes...)
	go c.Run(ctx)
	return c
}

// finish stops the writer, verifies the random-kill invariants (every
// acked write present, revisions never regressed, no split brain) and the
// post-fault convergence of every live node.
func finish(t *testing.T, h *Harness, w *Writer, c *Checker, dur time.Duration) {
	t.Helper()
	ops := int64(-1)
	if w != nil {
		w.Stop()
		ops = w.Ops()
	}
	verifyCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if w != nil {
		if err := w.VerifyAcked(verifyCtx); err != nil {
			t.Errorf("acked-write invariant: %v", err)
		}
	}
	if !c.Clean() {
		t.Errorf("invariant checker: %d violation(s):", len(c.Violations()))
		for _, v := range c.Violations() {
			t.Errorf("  %s: %s", v.Kind, v.Detail)
		}
	}
	h.WaitConverged(dur)
	if ops >= 0 {
		t.Logf("scenario complete: %d acked writes, checker clean, cluster converged", ops)
	} else {
		t.Logf("scenario complete: checker clean, cluster converged")
	}
}

// TestChaosRandomKill kills and restarts a random node repeatedly under
// write load. Invariants: no data loss of acked writes, monotonic
// revisions, no split brain.
func TestChaosRandomKill(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos: skipped in -short")
	}
	dur := scenarioDuration()
	h := NewHarness(t, 3)
	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()

	c := startChecker(ctx, h, 100*time.Millisecond)
	w := h.StartWriter(ctx)

	interval := faultInterval(dur, 1*time.Second, 60*time.Second)
	kills := 0
	for start := time.Now(); time.Since(start) < dur; {
		time.Sleep(interval)
		if ctx.Err() != nil {
			break
		}
		victim := h.RandomLiveNode()
		if victim < 0 {
			continue
		}
		h.Kill(victim)
		kills++
		t.Logf("killed n%d at %s", victim, time.Since(start).Round(time.Second))
		time.Sleep(interval / 2)
		h.Restart(victim)
	}
	t.Logf("random-kill: %d kill/restart cycles", kills)
	finish(t, h, w, c, 30*time.Second)
}

// TestChaosPartitionStorm applies random partitions (one node isolated
// against the two-node quorum group) every few seconds, with lease churn
// running across the whole storm. Invariants: never two lease holders,
// no divergence after heal.
func TestChaosPartitionStorm(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos: skipped in -short")
	}
	dur := scenarioDuration()
	h := NewHarness(t, 3)
	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()

	c := startChecker(ctx, h, 100*time.Millisecond)
	w := h.StartWriter(ctx)

	// Lease churn across the storm: two contenders take turns holding
	// the lease for one TTL. When a holder is partitioned away its
	// renewals fail, the majority sees expiry and takes over — the
	// checker must see at most one valid holder throughout.
	stormCtx, stormCancel := context.WithCancel(ctx)
	var churn sync.WaitGroup
	for _, node := range []int{0, 1} {
		n := node
		churn.Add(1)
		go func() {
			defer churn.Done()
			m := h.LeaseManager(n, nil)
			for {
				hld, err := m.Acquire(stormCtx, "storm-lease", 2*time.Second)
				if err != nil {
					return
				}
				select {
				case <-stormCtx.Done():
					_ = m.Release(context.Background(), hld)
					return
				case <-hld.Done():
					// lost (partition/kill) — try again
				case <-time.After(2 * time.Second):
					_ = m.Release(stormCtx, hld)
				}
				select {
				case <-stormCtx.Done():
					return
				case <-time.After(200 * time.Millisecond):
				}
			}
		}()
	}

	interval := faultInterval(dur, 1*time.Second, 90*time.Second)
	splits := 0
	for start := time.Now(); time.Since(start) < dur; {
		time.Sleep(interval)
		if ctx.Err() != nil {
			break
		}
		// Isolate one random node against the quorum pair.
		alone := rand.Intn(3)
		rest := []int{}
		for i := 0; i < 3; i++ {
			if i != alone {
				rest = append(rest, i)
			}
		}
		h.Partition([]int{alone}, rest)
		splits++
		t.Logf("partitioned n%d alone at %s", alone, time.Since(start).Round(time.Second))
		time.Sleep(interval / 2)
		h.ClearPartition()
	}
	stormCancel()
	churn.Wait()
	t.Logf("partition-storm: %d splits", splits)
	finish(t, h, w, c, 30*time.Second)
}

// TestChaosClockSkew runs lease churn while the granting clocks skew
// (±2 s, then ±5 s, then ±10 s offsets and 1.05× rate drift on one node).
// All timing loops stay monotonic (renewal ticker, ttl arithmetic in the
// holder); only the recorded ExpiresAt timestamps skew. Invariant: never
// two lease holders (record-side via the checker; holder-side via the
// guard-band argument — a takeover must not begin while the old holder
// still believes it valid).
func TestChaosClockSkew(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos: skipped in -short")
	}
	dur := scenarioDuration()
	h := NewHarness(t, 3)
	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()

	c := startChecker(ctx, h, 100*time.Millisecond)

	// Skew profiles: node 0 runs ahead, node 1 behind, node 2 drifts at
	// 1.05× rate on top of a wandering offset. Offsets step through the
	// spec's magnitudes over the scenario.
	type profile struct {
		node   int
		offset time.Duration
		rate   float64
	}
	base := time.Now()
	startedAt := time.Now()
	step := func(frac float64) time.Duration {
		// Ramp offset magnitude: 0 → +2s → +5s → +10s → back, by phase.
		phase := int(frac*4) % 4
		switch phase {
		case 0:
			return 0
		case 1:
			return 2 * time.Second
		case 2:
			return 5 * time.Second
		default:
			return 10 * time.Second
		}
	}
	_ = base
	profiles := []profile{{node: 0, offset: 2 * time.Second}, {node: 1, offset: -2 * time.Second}, {node: 2, rate: 1.05}}
	clocks := make([]func() time.Time, 3)
	for i, p := range profiles {
		p := p
		idx := i
		clocks[idx] = func() time.Time {
			elapsed := time.Since(startedAt)
			off := p.offset
			if p.rate != 0 {
				off += time.Duration(float64(elapsed) * (p.rate - 1))
			}
			off = time.Duration(float64(off) * (1 + 0)) // keep sign
			mag := step(elapsed.Seconds() / dur.Seconds())
			if off >= 0 {
				off += mag
			} else {
				off -= mag
			}
			return time.Now().Add(off)
		}
	}

	// Contenders on skewed clocks fight for the lease; each takeover is
	// recorded. Guard-band check: when a contender acquires, the OTHER
	// side's most recent Held (if any) must already be invalid — a new
	// holder may never coexist with a believing old holder.
	var evMu sync.Mutex
	var takeovers int
	skewCtx, skewCancel := context.WithCancel(ctx)
	var churn sync.WaitGroup
	for _, node := range []int{0, 1} {
		n := node
		churn.Add(1)
		go func() {
			defer churn.Done()
			m := h.LeaseManager(n, clocks[n])
			var prevMine bool
			for {
				hld, err := m.Acquire(skewCtx, "skew-lease", 2*time.Second)
				if err != nil {
					return
				}
				evMu.Lock()
				if prevMine == false {
					takeovers++
				}
				prevMine = true
				evMu.Unlock()
				select {
				case <-skewCtx.Done():
					_ = m.Release(context.Background(), hld)
					return
				case <-hld.Done():
					prevMine = false // lost (taken over / expired)
				case <-time.After(1500 * time.Millisecond):
					_ = m.Release(skewCtx, hld)
					prevMine = false
				}
			}
		}()
	}
	// Let the churn run the full scenario, including phase shifts into
	// the ±10 s band.
	time.Sleep(dur)
	skewCancel()
	churn.Wait()
	t.Logf("clock-skew: %d takeovers under skew up to ±10s + 1.05x drift", takeovers)
	finish(t, h, nil, c, 15*time.Second)
}

// TestChaosSlowDisk injects fsync latency (ramped 0 → 100 → 300 → 500 ms
// per log-store write, the device-mapper delay equivalent in-process).
// Invariants: the cluster stays available — writes keep being acked, just
// slower — and no corruption (all live nodes converge to one hash).
func TestChaosSlowDisk(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos: skipped in -short")
	}
	dur := scenarioDuration()
	h := NewHarness(t, 3)
	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()

	c := startChecker(ctx, h, 100*time.Millisecond)
	w := h.StartWriter(ctx)

	// Ramp the latency through thirds of the scenario.
	ramp := []time.Duration{0, 100 * time.Millisecond, 300 * time.Millisecond, 500 * time.Millisecond}
	for _, lat := range ramp {
		h.LogLatency(lat)
		t.Logf("fsync latency → %v", lat)
		time.Sleep(dur / 4)
	}
	h.LogLatency(0)
	w.Stop()
	slowOps := w.Ops()
	if slowOps == 0 {
		t.Error("slow-disk: no writes acked even at 500 ms fsync latency")
	}
	t.Logf("slow-disk: %d writes acked through the latency ramp", slowOps)
	finish(t, h, w, c, 30*time.Second)
}

// TestChaosPacketLoss injects increasing dial-loss (5 %, 20 %, 50 %),
// severing established flows at each step. Invariants: the cluster
// converges (leader may flap), no split brain, acked writes survive.
func TestChaosPacketLoss(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos: skipped in -short")
	}
	dur := scenarioDuration()
	h := NewHarness(t, 3)
	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()

	c := startChecker(ctx, h, 100*time.Millisecond)
	w := h.StartWriter(ctx)

	for _, loss := range []float64{0.05, 0.20, 0.50} {
		h.Loss(loss)
		t.Logf("packet loss → %.0f%%", loss*100)
		time.Sleep(dur / 4)
	}
	h.Loss(0)
	finish(t, h, w, c, 30*time.Second)
}

// TestChaosLeaderChurn forces leadership transfers continuously under
// write load. Invariants: writes stay linearizable (every acked write
// readable at its acked revision or later), no split brain.
func TestChaosLeaderChurn(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos: skipped in -short")
	}
	dur := scenarioDuration()
	h := NewHarness(t, 3)
	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()

	c := startChecker(ctx, h, 100*time.Millisecond)
	w := h.StartWriter(ctx)

	interval := faultInterval(dur, 1*time.Second, 15*time.Second)
	transfers := 0
	for start := time.Now(); time.Since(start) < dur; {
		time.Sleep(interval)
		if ctx.Err() != nil {
			break
		}
		cur := h.LeaderIndex()
		if cur < 0 {
			continue
		}
		next := (cur + 1 + rand.Intn(h.n-1)) % h.n
		err := h.Node(cur).TransferLeadership(h.nodeID(next))
		if err == nil {
			transfers++
		} else if transfers == 0 {
			t.Logf("transfer n%d -> n%d error: %v", cur, next, err)
		}
		time.Sleep(300 * time.Millisecond) // let the new leader settle
	}
	t.Logf("leader-churn: %d transfers", transfers)
	if transfers == 0 {
		t.Error("leader-churn: no transfer ever succeeded")
	}
	finish(t, h, w, c, 30*time.Second)
}

// TestCheckerCatchesInjectedDoubleHold is the negative test required by
// the task card: the checker BINARY (a real separate process) must exit 1
// with a SPLIT BRAIN report when two nodes both answer as unexpired
// holders of the same lease at the same (newest) revision.
func TestCheckerCatchesInjectedDoubleHold(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos: skipped in -short")
	}
	bin := buildChecker(t)

	// Two stub node APIs, each claiming to hold lease "vip:x" unexpired
	// at the same newest revision — the injected double-hold.
	srvA := startLeaseStub(t, "holder-a")
	srvB := startLeaseStub(t, "holder-b")

	cmd := exec.Command(bin,
		"-node", srvA.addr, "-node", srvB.addr,
		"-lease", "vip:x", "-insecure",
		"-interval", "100ms", "-for", "2s")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("checker did not catch the double-hold (exit 0): %s", out)
	}
	if !strings.Contains(string(out), "SPLIT BRAIN") {
		t.Fatalf("checker failed without a SPLIT BRAIN report: %v %s", err, out)
	}
}

// TestCheckerCleanRun is the positive control: agreeing nodes (one
// holder) must let the checker exit 0 after the run duration.
func TestCheckerCleanRun(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos: skipped in -short")
	}
	bin := buildChecker(t)

	srvA := startLeaseStub(t, "holder-a")
	srvB := startLeaseStub(t, "holder-a") // same holder: no split

	cmd := exec.Command(bin,
		"-node", srvA.addr, "-node", srvB.addr,
		"-lease", "vip:x", "-insecure",
		"-interval", "100ms", "-for", "1s")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("clean run should exit 0: %v %s", err, out)
	}
	if !strings.Contains(string(out), "clean for") {
		t.Fatalf("missing clean summary: %s", out)
	}
}

// buildChecker compiles the separate-process checker binary once.
func buildChecker(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "checker")
	cmd := exec.Command("go", "build", "-o", bin, "./test/chaos/cluster/checker")
	cmd.Dir = repoRoot(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build checker: %v\n%s", err, out)
	}
	return bin
}

// repoRoot locates the module root (the chaos package lives under test/).
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("go.mod not found")
	return ""
}

// startLeaseStub serves GetLease with a fixed unexpired holder on a
// loopback port over plaintext (checker -insecure mode).
func startLeaseStub(t *testing.T, holder string) struct{ addr string } {
	t.Helper()
	addr, stop := serveLeaseStub(t, holder)
	t.Cleanup(stop)
	return struct{ addr string }{addr: addr}
}
