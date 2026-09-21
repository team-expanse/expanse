package controller

import (
	"context"
	"slices"
	"testing"

	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/store"
)

func (r *rebuildRig) op(kind, key, body string) {
	r.t.Helper()
	if _, err := r.st.Put(context.Background(), store.Key("/volumes/_ops/"+kind+"/"+key), []byte(body)); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rebuildRig) hasOp(kind, key string) bool {
	_, err := r.st.Get(context.Background(), store.Key("/volumes/_ops/"+kind+"/"+key))
	return err == nil
}

func (r *rebuildRig) gone(node string) []string {
	r.t.Helper()
	ids, err := storage.ListGone(context.Background(), r.st, node)
	if err != nil {
		r.t.Fatal(err)
	}
	return ids
}

func TestForcedDeleteAbandonsTheReplicasOfDownNodesAndMarksThemGone(t *testing.T) {
	r := newRig(t)
	seedMesh(r.st, "n1", "n2") // n3 is down
	r.volume("vol-a", 3, "n1", "n2", "n3")
	r.op("delete", "vol-a", `{"target":"vol-a","force":true}`)
	r.round(0)
	if got := load(t, r.st, "vol-a").State; got != storage.StateDeleting {
		t.Fatalf("state = %s, want Deleting", got)
	}
	if got := r.hosts("vol-a"); !slices.Equal(got, []string{"n1", "n2"}) {
		t.Errorf("placement %v: only the down node's row may be dropped", got)
	}
	if got := r.gone("n3"); !slices.Equal(got, []string{"vol-a"}) {
		t.Errorf("n3 marks = %v; without one the node keeps the replica when it returns", got)
	}
	if got := append(r.gone("n1"), r.gone("n2")...); len(got) != 0 {
		t.Errorf("live nodes were marked gone: %v", got)
	}
	if r.hasOp("delete", "vol-a") {
		t.Error("request not consumed")
	}
}

func TestAForcedDeleteFinishesOnceTheLiveNodesHaveReleased(t *testing.T) {
	r := newRig(t)
	seedMesh(r.st, "n1", "n2")
	r.volume("vol-a", 3, "n1", "n2", "n3")
	r.op("delete", "vol-a", `{"target":"vol-a","force":true}`)
	r.round(0)
	edit(t, r.st, "vol-a", func(s *storage.Status) { s.Placement = nil }) // n1 and n2 report done
	r.round(0)
	if _, err := storage.LoadSpec(context.Background(), r.st, "vol-a"); err == nil {
		t.Error("the volume was not finalised")
	}
	if got := r.gone("n3"); !slices.Equal(got, []string{"vol-a"}) {
		t.Errorf("n3's mark must outlive the volume's records: %v", got)
	}
}

func TestAPlainDeleteStillWaitsForADownNode(t *testing.T) {
	r := newRig(t)
	seedMesh(r.st, "n1", "n2")
	r.volume("vol-a", 3, "n1", "n2", "n3")
	r.op("delete", "vol-a", `{"target":"vol-a"}`)
	r.round(0)
	if got := r.hosts("vol-a"); len(got) != 3 {
		t.Errorf("placement %v: a plain delete must not drop a row it has no proof for", got)
	}
	if got := r.gone("n3"); len(got) != 0 {
		t.Errorf("a plain delete marked n3 gone: %v", got)
	}
}

func TestRetireGivesUpADownNodesReplicaAndItsRequests(t *testing.T) {
	r := newRig(t)
	seedMesh(r.st, "n1", "n2") // n3 is down, and no spare exists
	r.volume("vol-a", 3, "n1", "n2", "n3")
	n3 := r.allocation("vol-a").NodeIDs["n3"]
	r.op("resolve", "vol-a/n3", `{"survivor":"n1"}`)
	r.op("resync", "vol-a/n3", `{}`)
	r.op("resolve", "vol-a/n1", `{"survivor":"n1"}`)
	r.op("retire", "vol-a", `{"target":"vol-a","node":"n3"}`)
	r.round(0)
	al := r.allocation("vol-a")
	if got := r.hosts("vol-a"); !slices.Equal(got, []string{"n1", "n2"}) || !slices.Equal(hostsOf(al), []string{"n1", "n2"}) || !slices.Equal(al.Retired, []int{n3}) {
		t.Errorf("want n3 dropped and id %d retired at once: placement %v, allocation %+v", n3, got, al)
	}
	if r.hasOp("resolve", "vol-a/n3") || r.hasOp("resync", "vol-a/n3") {
		t.Error("the dead node's requests are left; a survivor would wait for them forever")
	}
	if !r.hasOp("resolve", "vol-a/n1") {
		t.Error("a live node's request was dropped")
	}
	if r.hasOp("retire", "vol-a") {
		t.Error("request not consumed")
	}
}

func TestRetireRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		mesh []string
		node string
		set  func(*storage.Status)
	}{
		"a live node":                {[]string{"n1", "n2", "n3"}, "n3", nil},
		"a node holding no replica":  {[]string{"n1", "n2"}, "n9", nil},
		"no other replica reachable": {[]string{}, "n3", nil},
		"a volume being deleted":     {[]string{"n1", "n2"}, "n3", func(s *storage.Status) { s.State = storage.StateDeleting }},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			seedMesh(r.st, tc.mesh...)
			r.volume("vol-a", 3, "n1", "n2", "n3")
			if tc.set != nil {
				edit(t, r.st, "vol-a", tc.set)
			}
			before := r.allocation("vol-a")
			r.op("retire", "vol-a", `{"target":"vol-a","node":"`+tc.node+`"}`)
			r.round(0)
			if after := r.allocation("vol-a"); !slices.Equal(hostsOf(after), hostsOf(before)) || len(after.Retired) != 0 {
				t.Errorf("the allocation changed: %+v", after)
			}
			if r.hasOp("retire", "vol-a") {
				t.Error("a refused request stays queued and is refused every round")
			}
		})
	}
}

func TestRetireFinishesWhereACrashLeftIt(t *testing.T) {
	r := newRig(t)
	seedMesh(r.st, "n1", "n2")
	r.volume("vol-a", 3, "n1", "n2", "n3")
	if _, err := r.alloc.RetireNode(context.Background(), "vol-a", "n3"); err != nil { // crashed before the rows moved
		t.Fatal(err)
	}
	r.op("retire", "vol-a", `{"target":"vol-a","node":"n3"}`)
	r.round(0)
	if got := r.hosts("vol-a"); !slices.Equal(got, []string{"n1", "n2"}) {
		t.Errorf("placement %v", got)
	}
	if r.hasOp("retire", "vol-a") {
		t.Error("request not consumed")
	}
}

func TestRetireWorksOnADivergedVolume(t *testing.T) {
	r := newRig(t)
	seedMesh(r.st, "n1", "n2")
	r.volume("vol-a", 3, "n1", "n2", "n3")
	edit(t, r.st, "vol-a", func(s *storage.Status) { s.State = storage.StateNeedsManualRecovery })
	r.op("retire", "vol-a", `{"target":"vol-a","node":"n3"}`)
	r.round(0)
	if got := r.hosts("vol-a"); !slices.Equal(got, []string{"n1", "n2"}) {
		t.Errorf("placement %v: a dead replica must not hold a diverged volume's resolve back", got)
	}
	if got := load(t, r.st, "vol-a").State; got != storage.StateNeedsManualRecovery {
		t.Errorf("state = %s: only the operator's resolve leaves manual recovery", got)
	}
}

func TestRequestsForVolumesThatNoLongerExistAreSwept(t *testing.T) {
	r := newRig(t)
	seedMesh(r.st, "n1", "n2")
	r.volume("vol-a", 2, "n1", "n2")
	stale := map[string]string{
		"delete": "vol-x", "move-primary": "vol-x", "resize": "vol-x", "retire": "vol-x",
		"snapshot": "vol-x", "restore": "vol-x", "verify": "vol-x",
		"resolve": "vol-x/n1", "resync": "vol-x/n2",
	}
	for kind, key := range stale {
		r.op(kind, key, `{}`)
	}
	r.op("verify", "vol-a", `{}`)
	r.op("resolve", "vol-a/n1", `{}`)
	r.op("verify", "vol-a-longer", `{}`) // a name that only starts like a volume's is not that volume
	r.round(0)
	for kind, key := range stale {
		if r.hasOp(kind, key) {
			t.Errorf("%s/%s outlived its volume", kind, key)
		}
	}
	if !r.hasOp("verify", "vol-a") || !r.hasOp("resolve", "vol-a/n1") {
		t.Error("a request of a volume that exists was swept")
	}
	if r.hasOp("verify", "vol-a-longer") {
		t.Error("prefix matching kept another volume's request")
	}
}

func TestRequestsAddressedByVolumeNameAreNotSwept(t *testing.T) {
	c, st := newPlacer(t, "n1", "n2")
	request(t, st, "data", 2)
	reconcile(t, c)
	if _, err := st.Put(context.Background(), "/volumes/_ops/verify/data", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	reconcile(t, c)
	if _, err := st.Get(context.Background(), "/volumes/_ops/verify/data"); err != nil {
		t.Error("a request naming an existing volume was swept")
	}
}
