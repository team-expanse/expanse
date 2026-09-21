package volume

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/expanse/expanse/internal/storage"
)

// diverge leaves a two-replica volume the way B6 does: marked here and in the store.
func diverge(t *testing.T) *nodeRig {
	t.Helper()
	r := newNodeRig(t)
	r.place(t, "n1", "n1", "n2")
	r.setState(t, storage.StateNeedsManualRecovery)
	runHandler(t, r.marks, vol)
	return r
}

// choose queues the operator's request for each node, as the CLI does.
func (r *nodeRig) choose(t *testing.T, survivor string, nodes ...string) {
	t.Helper()
	raw, err := json.Marshal(resolveOp{Survivor: survivor})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if _, err := r.st.Put(context.Background(), resolveKey(vol, n), raw); err != nil {
			t.Fatal(err)
		}
	}
}

func (r *nodeRig) queued(t *testing.T, node string) bool {
	t.Helper()
	_, err := r.st.Get(context.Background(), resolveKey(vol, node))
	return err == nil
}

func (r *nodeRig) marked(t *testing.T) bool {
	t.Helper()
	ok, err := r.marks.Marked(vol)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestTheVictimDiscardsItsDataAndForgetsTheSplitBrain(t *testing.T) {
	r := diverge(t)
	r.choose(t, "n2", "n1", "n2")
	r.mustSync(t)
	if !reflect.DeepEqual(r.conv.rejoined, []bool{true}) {
		t.Errorf("rejoined %v, want one discarding rejoin", r.conv.rejoined)
	}
	if r.queued(t, "n1") || r.marked(t) {
		t.Errorf("request or mark left behind: queued=%v marked=%v", r.queued(t, "n1"), r.marked(t))
	}
	if got := r.status(t).State; got != storage.StateNeedsManualRecovery {
		t.Errorf("state = %s: only the survivor returns the volume to service", got)
	}
}

func TestTheVictimStopsLeading(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n1", "n1", "n2")
	r.mustSync(t)
	waitStarted(t, r.lead)
	runHandler(t, r.marks, vol)
	r.setState(t, storage.StateNeedsManualRecovery)
	r.choose(t, "n2", "n1", "n2")
	r.mustSync(t)
	if r.lead.isRunning(vol) {
		t.Error("the victim kept leading")
	}
}

func TestTheSurvivorWaitsUntilEveryOtherReplicaHasDiscarded(t *testing.T) {
	r := diverge(t)
	r.node.Self = "n2"
	r.choose(t, "n2", "n1", "n2")
	r.mustSync(t)
	if len(r.conv.rejoined) != 0 || !r.queued(t, "n2") || !r.marked(t) {
		t.Errorf("the survivor went ahead: rejoined=%v queued=%v marked=%v", r.conv.rejoined, r.queued(t, "n2"), r.marked(t))
	}
	if got := r.status(t).State; got != storage.StateNeedsManualRecovery {
		t.Errorf("state = %s", got)
	}
}

func TestTheSurvivorKeepsItsDataAndReturnsTheVolumeToService(t *testing.T) {
	r := diverge(t)
	r.node.Self = "n2"
	r.choose(t, "n2", "n2")
	r.mustSync(t)
	if !reflect.DeepEqual(r.conv.rejoined, []bool{false}) {
		t.Errorf("rejoined %v, want one keeping rejoin", r.conv.rejoined)
	}
	st := r.status(t)
	if st.State != storage.StateDegraded || st.Primary != "n2" {
		t.Errorf("state %s primary %s, want Degraded and n2 until the controller sees both replicas healthy", st.State, st.Primary)
	}
	if r.queued(t, "n2") || r.marked(t) {
		t.Errorf("request or mark left behind: queued=%v marked=%v", r.queued(t, "n2"), r.marked(t))
	}
}

// The survivor may finish before a victim that was down does; the victim's mark must not undo it.
func TestAVictimThatWasDownDiscardsAfterTheVolumeIsBackInService(t *testing.T) {
	r := diverge(t)
	r.setState(t, storage.StateDegraded)
	r.choose(t, "n2", "n1")
	r.mustSync(t)
	if !reflect.DeepEqual(r.conv.rejoined, []bool{true}) || r.marked(t) {
		t.Errorf("rejoined %v marked %v", r.conv.rejoined, r.marked(t))
	}
	if got := r.status(t).State; got != storage.StateDegraded {
		t.Errorf("state = %s: the stale mark sent the volume back to manual recovery", got)
	}
}

func TestARequestNamingANodeThatHoldsNoReplicaIsDropped(t *testing.T) {
	for _, self := range []string{"n1", "n2"} {
		r := diverge(t)
		r.node.Self = self
		r.choose(t, "n9", self)
		r.mustSync(t)
		if len(r.conv.rejoined) != 0 || r.queued(t, self) {
			t.Errorf("%s: rejoined=%v queued=%v", self, r.conv.rejoined, r.queued(t, self))
		}
		if !r.marked(t) {
			t.Errorf("%s: the mark was cleared", self)
		}
	}
}

func TestARequestForAVolumeThatIsNotDivergedIsDropped(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n1", "n1", "n2")
	r.node.Self = "n2"
	r.choose(t, "n2", "n2")
	r.mustSync(t)
	st := r.status(t)
	if len(r.conv.rejoined) != 0 || r.queued(t, "n2") || st.State != storage.StateHealthy || st.Primary != "n1" {
		t.Errorf("rejoined=%v queued=%v state=%s primary=%s", r.conv.rejoined, r.queued(t, "n2"), st.State, st.Primary)
	}
}

func TestASurvivorThatAlreadyFinishedDropsItsLeftoverRequest(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n2", "n1", "n2")
	r.setState(t, storage.StateDegraded)
	r.node.Self = "n2"
	r.choose(t, "n2", "n2")
	r.mustSync(t)
	if len(r.conv.rejoined) != 0 || r.queued(t, "n2") {
		t.Errorf("rejoined=%v queued=%v", r.conv.rejoined, r.queued(t, "n2"))
	}
}

func TestAnUnreadableRequestIsDropped(t *testing.T) {
	r := diverge(t)
	if _, err := r.st.Put(context.Background(), resolveKey(vol, "n1"), []byte("{")); err != nil {
		t.Fatal(err)
	}
	r.mustSync(t)
	if r.queued(t, "n1") || len(r.conv.rejoined) != 0 {
		t.Errorf("queued=%v rejoined=%v", r.queued(t, "n1"), r.conv.rejoined)
	}
}

func TestAFailedRejoinKeepsTheRequestAndTheMarkForARetry(t *testing.T) {
	r := diverge(t)
	r.conv.rejoinErr = errors.New("device busy")
	r.choose(t, "n2", "n1", "n2")
	if err := r.sync(t); err == nil {
		t.Fatal("want the failure reported")
	}
	if !r.queued(t, "n1") || !r.marked(t) {
		t.Errorf("queued=%v marked=%v", r.queued(t, "n1"), r.marked(t))
	}
	r.conv.rejoinErr = nil
	r.mustSync(t)
	if r.queued(t, "n1") || r.marked(t) {
		t.Errorf("retry did not finish: queued=%v marked=%v", r.queued(t, "n1"), r.marked(t))
	}
}

func TestTheSurvivorDoesNotOverwriteADeleteRequestedMeanwhile(t *testing.T) {
	r := diverge(t)
	r.node.Self = "n2"
	r.choose(t, "n2", "n2")
	r.node.St = &deletingStore{Store: r.st}
	r.mustSync(t)
	if got := r.status(t).State; got != storage.StateDeleting {
		t.Errorf("state = %s: the resolution overwrote the delete", got)
	}
	if r.lead.isRunning(vol) || len(r.conv.desired) != 0 {
		t.Errorf("the pass went on with a volume being deleted: leading=%v reconciled=%d", r.lead.isRunning(vol), len(r.conv.desired))
	}
}

func TestOtherVolumesRequestsAreNotConsumed(t *testing.T) {
	r := diverge(t)
	raw, _ := json.Marshal(resolveOp{Survivor: "n2"})
	other := opKey(opResolve, "vol-zzzz/n1")
	if _, err := r.st.Put(context.Background(), other, raw); err != nil {
		t.Fatal(err)
	}
	r.mustSync(t)
	if _, err := r.st.Get(context.Background(), other); err != nil {
		t.Errorf("another volume's request was consumed: %v", err)
	}
}

// The survivor moved the primary away, but the victim's leader is still running until it is told to stop.
func TestAVictimStopsLeadingBeforeItDiscards(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n1", "n1", "n2")
	r.mustSync(t)
	waitStarted(t, r.lead)
	runHandler(t, r.marks, vol)
	r.setPrimary(t, "n2")
	r.setState(t, storage.StateDegraded)
	r.choose(t, "n2", "n1")
	leading := true
	r.conv.onRejoin = func() { leading = r.lead.isRunning(vol) }
	r.mustSync(t)
	if leading {
		t.Error("the victim was still leading when it discarded its data")
	}
}

func TestASurvivorLeavesAFreshSplitBrainMarkAlone(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n2", "n1", "n2")
	r.setState(t, storage.StateDegraded)
	runHandler(t, r.marks, vol) // the kernel reported a new split-brain after the resolution
	r.node.Self = "n2"
	r.choose(t, "n2", "n2")
	r.mustSync(t)
	if len(r.conv.rejoined) != 0 {
		t.Errorf("rejoined %v: a leftover request undid a fresh split-brain", r.conv.rejoined)
	}
	if got := r.status(t).State; got != storage.StateNeedsManualRecovery {
		t.Errorf("state = %s, want the fresh split-brain recorded", got)
	}
}
