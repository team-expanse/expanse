// Package network extends the §6 chaos suite with the Phase 05
// networking scenarios. It reuses the test/chaos/cluster harness
// (in-process raftstore cluster with fault injection) — no parallel
// infrastructure, per the T23 card.
//
// The VIP duplicate invariant runs on the lease layer: a VIP holder is
// exactly a node holding lease "vip:<addr>" (internal/network/vip),
// and two valid holders of that lease = two nodes configuring the
// address = the T09 duplicate. The checker's split-brain detection on
// the VIP lease key therefore IS the VIP duplicate checker, and the
// partition storm below runs it continuously for the whole budget
// window instead of a single failover event.
package network

import (
	"context"
	"sync"
	"testing"
	"time"

	chaos "github.com/expanse/expanse/test/chaos/cluster"

	"github.com/expanse/expanse/internal/network/mesh"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	pbproto "google.golang.org/protobuf/proto"
)

// runChecker is the package-local wrapper over the (unexported)
// cluster-package helper: poll every live node until ctx is done.
func runChecker(ctx context.Context, h *chaos.Harness, interval time.Duration) *chaos.Checker {
	probes := make([]chaos.NodeProbe, 3)
	for i := range probes {
		probes[i] = h.HarnessLeaseProbe(i)
	}
	c := chaos.NewChecker(interval, probes...)
	go c.Run(ctx)
	return c
}

// assertClean fails the test if the checker recorded any violation.
func assertClean(t *testing.T, c *chaos.Checker) {
	t.Helper()
	if !c.Clean() {
		vs := c.Violations()
		t.Errorf("invariant checker: %d violation(s):", len(vs))
		for _, v := range vs {
			t.Errorf("  %s: %s", v.Kind, v.Detail)
		}
	}
}

// TestChaosVIPPartitionStorm is the critical §6 chaos addition: the
// same partition storm as the lease scenario, but the churned lease is
// a VIP lease ("vip:192.168.1.100") and the duplicate checker watches
// it continuously for the whole budget window. Invariant: at most one
// valid VIP holder at any poll, through every split and heal.
func TestChaosVIPPartitionStorm(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos: skipped in -short")
	}
	dur := chaos.ScenarioDuration()
	h := chaos.NewHarness(t, 3)
	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()

	c := runChecker(ctx, h, 100*time.Millisecond)
	w := h.StartWriter(ctx)

	// Two VIP candidates take turns holding the VIP lease (this is the
	// holder behavior of vip.Holder, minus netlink): acquire, hold one
	// TTL, release or lose.
	const vipLease = "vip:192.168.1.100"
	stormCtx, stormCancel := context.WithCancel(ctx)
	var churn sync.WaitGroup
	for _, node := range []int{0, 1} {
		n := node
		churn.Add(1)
		go func() {
			defer churn.Done()
			m := h.LeaseManager(n, nil)
			for {
				hld, err := m.Acquire(stormCtx, vipLease, 2*time.Second)
				if err != nil {
					return
				}
				select {
				case <-stormCtx.Done():
					_ = m.Release(context.Background(), hld)
					return
				case <-hld.Done():
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

	interval := chaos.FaultInterval(dur, 1*time.Second, 90*time.Second)
	splits := 0
	for start := time.Now(); time.Since(start) < dur; {
		time.Sleep(interval)
		if ctx.Err() != nil {
			break
		}
		alone := 2 // keep both VIP contenders in the quorum pair
		rest := []int{0, 1}
		if splits%3 == 2 { // every third split: isolate a contender
			alone, rest = 0, []int{1, 2}
		}
		h.Partition([]int{alone}, rest)
		splits++
		t.Logf("partitioned n%d alone at %s", alone, time.Since(start).Round(time.Second))
		time.Sleep(interval / 2)
		h.ClearPartition()
	}
	stormCancel()
	churn.Wait()
	t.Logf("vip-partition-storm: %d splits", splits)

	w.Stop()
	verifyCtx, vcancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer vcancel()
	if err := w.VerifyAcked(verifyCtx); err != nil {
		t.Errorf("acked-write invariant: %v", err)
	}
	assertClean(t, c)
	h.WaitConverged(30 * time.Second)
}

// TestChaosReplicaKillSustainedLoad kills a random node every few
// seconds while a client-shaped load loop runs sustained writes. Each
// op gets a bounded deadline; a failure is an op that does not
// complete within it. Invariant: client-observed error rate < 0.1%
// (§6) — election timeouts must fit well inside the op deadline — and
// the checker stays clean.
func TestChaosReplicaKillSustainedLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos: skipped in -short")
	}
	dur := chaos.ScenarioDuration()
	h := chaos.NewHarness(t, 3)
	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()

	c := runChecker(ctx, h, 100*time.Millisecond)

	const opDeadline = 3 * time.Second // >> election timeout (~1–2 s)
	var attempts, failures int64
	var mu sync.Mutex
	loadCtx, loadCancel := context.WithCancel(ctx)
	var load sync.WaitGroup
	for worker := 0; worker < 2; worker++ {
		load.Add(1)
		go func() {
			defer load.Done()
			seq := 0
			for {
				seq++
				opCtx, opCancel := context.WithTimeout(loadCtx, opDeadline)
				_, err := h.Put(opCtx, chaos.LoadKey(worker, seq), []byte("v"))
				opCancel()
				mu.Lock()
				attempts++
				if err != nil {
					failures++
				}
				mu.Unlock()
				if loadCtx.Err() != nil {
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
		}()
	}

	interval := chaos.FaultInterval(dur, 1*time.Second, 60*time.Second)
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
		time.Sleep(interval / 2)
		h.Restart(victim)
	}
	loadCancel()
	load.Wait()
	t.Logf("replica-kill: %d kill/restart cycles, %d ops, %d failures", kills, attempts, failures)

	if attempts == 0 {
		t.Fatal("no load attempts recorded")
	}
	rate := float64(failures) / float64(attempts)
	if rate >= 0.001 {
		t.Errorf("error rate %.4f%% >= 0.1%% (%d/%d)", rate*100, failures, attempts)
	}
	assertClean(t, c)
	h.WaitConverged(30 * time.Second)
}

// TestChaosWGKeyRotation rotates a node's WireGuard keypair (new
// private key → republished peer record) while write load runs, then
// asserts every node's reconciler converges to the new key in ONE
// reconcile pass per node — the in-process form of "zero sustained
// connectivity loss beyond one reconcile pass" (§6).
func TestChaosWGKeyRotation(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos: skipped in -short")
	}
	dur := chaos.ScenarioDuration()
	h := chaos.NewHarness(t, 3)
	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()

	c := runChecker(ctx, h, 100*time.Millisecond)
	w := h.StartWriter(ctx)

	// One fake controller + reconciler per node, pre-converged with
	// the initial peer records.
	type nodeMesh struct {
		ctrl *fakeCtrl
		rec  *mesh.Reconciler
	}
	nodes := make([]nodeMesh, 3)
	pub := func(i int) string { return h.PeerRecord(i).PublicKey }
	publishAll := func() {
		for i := range nodes {
			rec := h.PeerRecord(i)
			b, err := pbproto.Marshal(rec)
			if err != nil {
				t.Fatalf("marshal peer n%d: %v", i, err)
			}
			if _, err := h.Put(ctx, mesh.PublicKeyKey(h.NodeID(i)), b); err != nil {
				t.Fatalf("publish n%d: %v", i, err)
			}
		}
	}
	publishAll()
	// Reconcilers read the store through the leader: List is
	// linearizable by default and a follower's Barrier fails in the
	// in-process harness (production reconcilers read a watcher cache).
	lead := h.Leader()
	if lead == nil {
		t.Fatal("no leader for reconcilers")
	}
	for i := range nodes {
		nodes[i].ctrl = newFakeCtrl()
		nodes[i].rec = mesh.NewReconciler(lead, nodes[i].ctrl, mesh.Config{})
		if err := nodes[i].rec.Reconcile(ctx); err != nil {
			t.Fatalf("initial reconcile n%d: %v", i, err)
		}
	}

	// Rotate node 1's keypair under load: new key file → new record.
	oldKey := pub(1)
	newPriv, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	h.RotatePeerKey(1, newPriv.String())
	publishAll() // republishes every record, n1's with the new key

	for i := range nodes {
		if err := nodes[i].rec.Reconcile(ctx); err != nil {
			t.Fatalf("post-rotation reconcile n%d: %v", i, err)
		}
		if i == 1 {
			continue // self peer is not configured on its own device
		}
		if !nodes[i].ctrl.HasPeer(newPriv.PublicKey().String()) {
			t.Errorf("n%d did not converge to rotated key in one pass", i)
		}
		if nodes[i].ctrl.HasPeer(oldKey) {
			t.Errorf("n%d still holds stale key %s after one pass", i, oldKey)
		}
	}

	w.Stop()
	verifyCtx, vcancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer vcancel()
	if err := w.VerifyAcked(verifyCtx); err != nil {
		t.Errorf("acked-write invariant: %v", err)
	}
	assertClean(t, c)
	h.WaitConverged(30 * time.Second)
}
