package volume

import (
	"context"
	"encoding/json"
	"testing"

	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/store"
)

func (f *fakeConverger) Snapshot(_ context.Context, res, snap string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.snapErr != nil {
		return f.snapErr
	}
	f.snapped = append(f.snapped, res+"/"+snap)
	return nil
}

func (f *fakeConverger) Restore(_ context.Context, d Desired, snap string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.restoreErr != nil {
		return f.restoreErr
	}
	f.restored = append(f.restored, d.Name+"/"+snap)
	return nil
}

func (r *nodeRig) putOp(t *testing.T, kind, snap string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"target": "data", "name": snap})
	if _, err := r.st.Put(context.Background(), store.Key("/volumes/_ops/"+kind+"/"+vol), raw); err != nil {
		t.Fatal(err)
	}
}

func (r *nodeRig) opPending(kind string) bool {
	_, err := r.st.Get(context.Background(), store.Key("/volumes/_ops/"+kind+"/"+vol))
	return err == nil
}

func (r *nodeRig) records(t *testing.T) []storage.SnapshotRecord {
	t.Helper()
	recs, err := storage.ListSnapshotRecords(context.Background(), r.st, vol)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func (r *nodeRig) recordSnapshot(t *testing.T, name, node string) {
	t.Helper()
	if err := storage.PutSnapshot(context.Background(), r.st, vol, storage.SnapshotRecord{Name: name, Node: node}); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotOpOnThePrimaryTakesAndRecordsTheSnapshotThenClearsTheOp(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n1", "n1", "n2", "n3")
	r.putOp(t, "snapshot", "before")
	r.mustSync(t)
	if len(r.conv.snapped) != 1 || r.conv.snapped[0] != vol+"/before" {
		t.Errorf("snapped %v, want [%s/before]", r.conv.snapped, vol)
	}
	if recs := r.records(t); len(recs) != 1 || recs[0].Name != "before" || recs[0].Node != "n1" || recs[0].CreatedAt.IsZero() {
		t.Errorf("records %+v, want before held by n1 with a time", recs)
	}
	if r.opPending("snapshot") {
		t.Error("the op was not cleared")
	}
}

func TestSnapshotOpWaitsForTheNodeThatIsTheVolumesPrimary(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n2", "n1", "n2", "n3")
	r.putOp(t, "snapshot", "before")
	r.mustSync(t)
	if len(r.conv.snapped) != 0 || !r.opPending("snapshot") {
		t.Errorf("a secondary acted on the op: snapped %v, pending %v", r.conv.snapped, r.opPending("snapshot"))
	}
}

func TestSnapshotOpRefusalsDropTheOpAndChangeNothing(t *testing.T) {
	cases := map[string]func(*nodeRig, *testing.T) string{
		"invalid name":         func(*nodeRig, *testing.T) string { return "Bad Name" },
		"held by another node": func(r *nodeRig, t *testing.T) string { r.recordSnapshot(t, "dup", "n2"); return "dup" },
		"not a thin volume error": func(r *nodeRig, _ *testing.T) string {
			r.conv.snapErr = experrors.New(experrors.KindInvalid, "t", "thick")
			return "s"
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			r := newNodeRig(t)
			r.place(t, "n1", "n1", "n2", "n3")
			r.putOp(t, "snapshot", setup(r, t))
			before := len(r.records(t))
			r.mustSync(t)
			if len(r.conv.snapped) != 0 || len(r.records(t)) != before || r.opPending("snapshot") {
				t.Errorf("snapped %v, records %d→%d, pending %v", r.conv.snapped, before, len(r.records(t)), r.opPending("snapshot"))
			}
		})
	}
}

func TestSnapshotOpThatFailsTransientlyIsRetriedAndRecordsNothingUntilItWorks(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n1", "n1", "n2", "n3")
	r.putOp(t, "snapshot", "before")
	r.conv.snapErr = experrors.New(experrors.KindInternal, "t", "lvm hiccup")
	if err := r.sync(t); err == nil {
		t.Error("the failure was swallowed")
	}
	if len(r.records(t)) != 0 || !r.opPending("snapshot") {
		t.Fatalf("records %v, pending %v; want none and pending", r.records(t), r.opPending("snapshot"))
	}
	r.conv.snapErr = nil
	r.mustSync(t)
	if len(r.records(t)) != 1 || r.opPending("snapshot") {
		t.Errorf("retry did not finish: records %v, pending %v", r.records(t), r.opPending("snapshot"))
	}
}

func TestRestoreOpOnTheHolderRestoresThenClearsTheOp(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n1", "n1", "n2", "n3")
	r.recordSnapshot(t, "before", "n1")
	r.putOp(t, "restore", "before")
	r.mustSync(t)
	if len(r.conv.restored) != 1 || r.conv.restored[0] != vol+"/before" {
		t.Errorf("restored %v, want [%s/before]", r.conv.restored, vol)
	}
	if r.opPending("restore") {
		t.Error("the op was not cleared")
	}
}

func TestRestoreOpIsDroppedWhenNoSuchSnapshotOrItLivesOnAnotherNode(t *testing.T) {
	for name, setup := range map[string]func(*nodeRig, *testing.T){
		"unknown":      func(*nodeRig, *testing.T) {},
		"other holder": func(r *nodeRig, t *testing.T) { r.recordSnapshot(t, "before", "n2") },
	} {
		t.Run(name, func(t *testing.T) {
			r := newNodeRig(t)
			r.place(t, "n1", "n1", "n2", "n3")
			setup(r, t)
			r.putOp(t, "restore", "before")
			r.mustSync(t)
			if len(r.conv.restored) != 0 || r.opPending("restore") {
				t.Errorf("restored %v, pending %v; want nothing restored and the op dropped", r.conv.restored, r.opPending("restore"))
			}
		})
	}
}

func TestRestoreOpWaitsUntilTheVolumeIsPrimaryHereThenRuns(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n1", "n1", "n2", "n3")
	r.recordSnapshot(t, "before", "n1")
	r.putOp(t, "restore", "before")
	r.conv.restoreErr = experrors.New(experrors.KindConflict, "t", "secondary here")
	if err := r.sync(t); err == nil {
		t.Error("a refused restore was not reported")
	}
	if !r.opPending("restore") {
		t.Fatal("the op was dropped, so the restore would never run")
	}
	r.conv.restoreErr = nil
	r.mustSync(t)
	if len(r.conv.restored) != 1 || r.opPending("restore") {
		t.Errorf("restored %v, pending %v", r.conv.restored, r.opPending("restore"))
	}
}

func TestRestoreOpIsLeftAloneOnANodeThatIsNotThePrimary(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n2", "n1", "n2", "n3")
	r.recordSnapshot(t, "before", "n1")
	r.putOp(t, "restore", "before")
	r.mustSync(t)
	if len(r.conv.restored) != 0 || !r.opPending("restore") {
		t.Errorf("restored %v, pending %v", r.conv.restored, r.opPending("restore"))
	}
}
