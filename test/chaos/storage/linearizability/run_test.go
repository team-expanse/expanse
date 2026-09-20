package linearizability

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/expanse/expanse/test/chaos/exvol"
)

const (
	volID        = "vol-linear"
	minVolSize   = 64 << 20
	nClients     = 6
	keyWindow    = 8  // concurrently hot blocks
	opsPerKey    = 48 // bounds the exact checker's per-register work
	checkSteps   = 5_000_000
	settleMargin = 200 // blocks below the hot window still treated as possibly in flight
)

// runDuration: RUN_CHAOS=1 → 1 hour (§5 nightly); CHAOS_DURATION
// overrides both; default 45 s for local runs.
func runDuration() time.Duration {
	if os.Getenv("RUN_CHAOS") == "1" {
		return time.Hour
	}
	if d, err := time.ParseDuration(os.Getenv("CHAOS_DURATION")); err == nil && d > 0 {
		return d
	}
	return 45 * time.Second
}

// volumeSize gives a run enough fresh blocks (each retires after
// opsPerKey ops, ~10 blocks/s at the throttled client rate, 4x margin);
// kept modest because a full resync copies the whole file.
func volumeSize(d time.Duration) int64 {
	n := int64(d.Seconds()) * (160 << 10)
	if n < minVolSize {
		return minVolSize
	}
	return n &^ (BlockSize - 1)
}

// TestLinearizabilityUnderFaults is the T21 release gate: concurrent
// clients + a fault nemesis, then an exact linearizability check of the
// full history. Any acked-write loss fails it.
func TestLinearizabilityUnderFaults(t *testing.T) {
	nodes := []string{"n1", "n2", "n3"}
	dur := runDuration()
	size := volumeSize(dur)
	c := exvol.NewFaultCluster(t, nodes...)
	c.CreateVolume(volID, uint64(size), nodes)

	rec := NewRecorder()
	pool := newKeyPool(keyWindow, opsPerKey, int(size/BlockSize))
	w := NewWorkload(c, volID, pool, rec, nodes)
	nem := NewNemesis(c, volID, nodes, t.Logf)
	// Finished blocks must be byte-identical whenever the cluster is healthy: catches
	// silent divergence when it happens instead of at the end of the hour.
	nem.SetInvariant(func() string { return replicaDiff(c, pool.settledKeys(settleMargin)) })

	waitPrimaryServes(t, c, w)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, nClients)
	for i := 0; i < nClients; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			if err := w.Run(id, stop); err != nil {
				errs <- err
			}
		}(i)
	}
	nemDone := make(chan struct{})
	go func() { defer close(nemDone); nem.Run(stop) }()

	select {
	case <-time.After(dur):
	case err := <-errs:
		t.Error(err)
	case <-nem.Failed():
		t.Errorf("replicas diverged while the cluster was healthy: %s", nem.Failure())
	}
	close(stop)
	wg.Wait()
	<-nemDone

	if err := nem.Settle(); err != nil {
		t.Fatal(err)
	}
	finalReads(t, c, w, pool, nem)
	convergeErr := assertReplicasConverge(c, pool)

	ops, violations := rec.History()
	for _, v := range violations {
		t.Errorf("read returned data no client ever sent: %s", v)
	}
	writes, indet, reads := tally(ops)
	t.Logf("history: %d ops (%d acked writes, %d indeterminate writes, %d reads) over %d blocks; faults: %v",
		len(ops), writes, indet, reads, pool.usedKeys(), nem.Counts())
	if writes == 0 {
		t.Fatal("no writes were acked: the run proved nothing")
	}
	res, err := CheckHistory(ops, checkSteps)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Errorf("NOT LINEARIZABLE at block %d (acked-write loss is a release blocker):\n%s", res.Key, res.Reason)
	}
	if convergeErr != nil {
		t.Errorf("replicas never converged: %v\nstatus: %+v\nnemesis stuck: %q", convergeErr, c.Status(volID).Placement, nem.Stuck())
	}
}

func tally(ops []Op) (writes, indet, reads int) {
	for _, o := range ops {
		switch {
		case o.Kind == Read:
			reads++
		case o.Indeterminate:
			indet++
		default:
			writes++
		}
	}
	return
}

// waitPrimaryServes waits until the volume's primary acks a write, so
// the run does not start inside initial promotion.
func waitPrimaryServes(t *testing.T, c *exvol.Cluster, w *Workload) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		n := c.Node(c.Status(volID).Primary)
		if n != nil && n.Runtime().WriteOp(volID, 0, make([]byte, BlockSize)) == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("volume primary never came up")
}

// finalReads reads back every block ever used through the settled
// primary, recording them as ordinary ops: this is what proves every
// acked write is still readable once all faults are gone.
func finalReads(t *testing.T, c *exvol.Cluster, w *Workload, pool *keyPool, nem *Nemesis) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for key := 0; key < pool.usedKeys(); key++ {
		for !w.read(-1, c.Node(c.Status(volID).Primary), key) {
			if time.Now().After(deadline) {
				last, _ := w.lastReadErr.Load().(string)
				t.Logf("goroutine dump: %s", DumpGoroutines())
				t.Fatalf("final read of block %d never succeeded: the volume did not recover (last read error: %s; nemesis: %q)",
					key, last, nem.Stuck())
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// assertReplicasConverge requires all three replicas to hold identical
// bytes for every used block (no silent divergence, no lost replication).
func assertReplicasConverge(c *exvol.Cluster, pool *keyPool) error {
	deadline := time.Now().Add(90 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		if last = replicaDiff(c, pool.usedKeys()); last == "" {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("%s", last)
}

func replicaDiff(c *exvol.Cluster, keys int) string {
	files := map[string]*os.File{}
	for _, n := range c.Nodes {
		f, err := os.Open(n.ZvolFile(volID))
		if err != nil {
			return fmt.Sprintf("%s: %v", n.ID, err)
		}
		defer f.Close() //nolint:errcheck
		files[n.ID] = f
	}
	bufs := map[string][]byte{}
	for _, n := range c.Nodes {
		bufs[n.ID] = make([]byte, BlockSize)
	}
	for key := 0; key < keys; key++ {
		for _, n := range c.Nodes {
			if _, err := files[n.ID].ReadAt(bufs[n.ID], int64(key)*BlockSize); err != nil {
				return fmt.Sprintf("%s block %d: %v", n.ID, key, err)
			}
		}
		for _, n := range c.Nodes[1:] {
			if !bytes.Equal(bufs[c.Nodes[0].ID], bufs[n.ID]) {
				return fmt.Sprintf("block %d differs; tokens by node: %s", key, tokens(c, key, bufs))
			}
		}
	}
	return ""
}

// tokens renders each node's token for a block ("n1=862 n2=860 n3=862").
func tokens(c *exvol.Cluster, key int, bufs map[string][]byte) string {
	var b strings.Builder
	for _, n := range c.Nodes {
		tok, err := DecodeBlock(key, bufs[n.ID])
		if err != nil {
			fmt.Fprintf(&b, "%s=<%v> ", n.ID, err)
			continue
		}
		fmt.Fprintf(&b, "%s=%d ", n.ID, tok)
	}
	return b.String()
}
