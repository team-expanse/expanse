package volume

import (
	"context"
	"reflect"
	"testing"

	experrors "github.com/expanse/expanse/internal/errors"
)

func (f *fakeConverger) Verify(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.checkErr != nil {
		return f.checkErr
	}
	f.verified = append(f.verified, name)
	return nil
}

func (f *fakeConverger) Resync(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.checkErr != nil {
		return f.checkErr
	}
	f.resynced = append(f.resynced, name)
	return nil
}

func (r *nodeRig) requestResync(t *testing.T, node string) {
	t.Helper()
	if _, err := r.st.Put(context.Background(), resyncKey(vol, node), []byte("{}")); err != nil {
		t.Fatal(err)
	}
}

func (r *nodeRig) resyncPending(node string) bool {
	_, err := r.st.Get(context.Background(), resyncKey(vol, node))
	return err == nil
}

func TestVerifyOnThePrimaryStartsItAndClearsTheRequest(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n1", "n1", "n2", "n3")
	r.putOp(t, "verify", "")
	r.mustSync(t)
	if !reflect.DeepEqual(r.conv.verified, []string{vol}) || r.opPending("verify") {
		t.Errorf("verified %v, pending %v; want one verify and no request left", r.conv.verified, r.opPending("verify"))
	}
}

func TestVerifyWaitsForTheNodeThatIsThePrimary(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n2", "n1", "n2", "n3")
	r.putOp(t, "verify", "")
	r.mustSync(t)
	if len(r.conv.verified) != 0 || !r.opPending("verify") {
		t.Errorf("a secondary acted: verified %v, pending %v", r.conv.verified, r.opPending("verify"))
	}
}

func TestARefusedVerifyIsDroppedAndAFailedOneRetried(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n1", "n1", "n2", "n3")
	r.putOp(t, "verify", "")

	r.conv.checkErr = experrors.New(experrors.KindInternal, "t", "drbdadm hiccup")
	if err := r.sync(t); err == nil || !r.opPending("verify") {
		t.Fatalf("a failure was swallowed or the request dropped: err %v, pending %v", err, r.opPending("verify"))
	}
	r.conv.checkErr = experrors.New(experrors.KindInvalid, "t", "a replica is behind")
	r.mustSync(t)
	if r.opPending("verify") {
		t.Error("a refused request stayed queued")
	}
}

func TestResyncRebuildsThisNodesReplicaAndClearsItsRequest(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n2", "n1", "n2")
	r.requestResync(t, "n1")
	r.mustSync(t)
	if !reflect.DeepEqual(r.conv.resynced, []string{vol}) || r.resyncPending("n1") {
		t.Errorf("resynced %v, pending %v; want one resync and no request left", r.conv.resynced, r.resyncPending("n1"))
	}
}

func TestResyncLeavesAnotherNodesRequestAlone(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n1", "n1", "n2")
	r.requestResync(t, "n2")
	r.mustSync(t)
	if len(r.conv.resynced) != 0 || !r.resyncPending("n2") {
		t.Errorf("resynced %v, n2 pending %v; want none and pending", r.conv.resynced, r.resyncPending("n2"))
	}
}

func TestARefusedResyncIsDroppedAndAFailedOneRetried(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n2", "n1", "n2")
	r.requestResync(t, "n1")

	r.conv.checkErr = experrors.New(experrors.KindInternal, "t", "drbdadm hiccup")
	if err := r.sync(t); err == nil || !r.resyncPending("n1") {
		t.Fatalf("a failure was swallowed or the request dropped: err %v, pending %v", err, r.resyncPending("n1"))
	}
	r.conv.checkErr = experrors.New(experrors.KindInvalid, "t", "it is the primary")
	r.mustSync(t)
	if r.resyncPending("n1") {
		t.Error("a refused request stayed queued")
	}
}

func TestResyncOfADivergedVolumeIsConsumedNotLeftToRunAfterTheResolution(t *testing.T) {
	r := diverge(t)
	r.requestResync(t, "n1")
	r.conv.checkErr = experrors.New(experrors.KindInvalid, "t", "no connected replica in sync")
	r.mustSync(t)
	if r.resyncPending("n1") {
		t.Error("the request survived the pass, so it could fire once the volume is resolved")
	}
}
