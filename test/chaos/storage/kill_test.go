package chaosstorage

import (
	"testing"
	"time"

	"github.com/expanse/expanse/test/chaos/exvol"
	"github.com/expanse/expanse/test/chaos/storage/linearizability"
)

// TestReplicaKillsUnderLoad kills replicas at random while clients write
// continuously. The guarantee under test: a write either becomes durable
// or fails with a clear error, and no acked write is ever lost.
func TestReplicaKillsUnderLoad(t *testing.T) {
	t.Run("secondary", func(t *testing.T) { runKills(t, []string{"kill-secondary"}, true) })
	t.Run("any-replica", func(t *testing.T) { runKills(t, []string{"kill-primary", "kill-secondary"}, false) })
}

const maxFailoverStall = 10 * time.Second // lease TTL 2 s plus recovery, generous for slow CI

func runKills(t *testing.T, faults []string, mustNotFail bool) {
	c := newVolume(t, exvol.Config{})
	nem := linearizability.NewNemesis(c, vol, nodeIDs, t.Logf)
	nem.Restrict(faults...)
	l := NewLoad(c, vol)

	stopLoad := l.Run(6, 20*time.Millisecond)
	stopNem := make(chan struct{})
	nemDone := make(chan struct{})
	go func() { defer close(nemDone); nem.Run(stopNem) }()
	time.Sleep(scenarioDuration(30 * time.Second))
	close(stopNem)
	<-nemDone
	stopLoad()
	if err := nem.Settle(); err != nil {
		t.Fatal(err)
	}

	all := l.Samples()
	t.Log(describe("writes", all), "faults:", nem.Counts())
	if why := nem.Stuck(); why != "" {
		t.Errorf("cluster never re-healed between faults: %s", why)
	}
	for _, e := range all.Errors() {
		if !ClearError(e.Err) {
			t.Errorf("write failed with an unclear error: %v", e.Err)
		}
	}
	if mustNotFail && len(all.Errors()) > 0 {
		t.Errorf("killing only secondaries must not fail client writes; %d failed, first: %v", len(all.Errors()), all.Errors()[0].Err)
	}
	if got := all.LongestStall(); got > maxFailoverStall {
		t.Errorf("volume made no progress for %v (limit %v)", got, maxFailoverStall)
	}
	for _, f := range faults {
		if nem.Counts()[f] == 0 {
			t.Errorf("fault %q never fired", f)
		}
	}
	waitHealthy(t, c, 90*time.Second)
	assertNoLoss(t, c, l)
}
