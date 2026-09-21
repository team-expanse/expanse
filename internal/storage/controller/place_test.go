package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	pbproto "google.golang.org/protobuf/proto"

	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/storage/drbd"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/boltstore"
	pb "github.com/expanse/expanse/proto"
)

func newPlacer(t *testing.T, nodes ...string) (*Controller, *boltstore.Store) {
	t.Helper()
	st := newStore(t)
	seedMesh(st, nodes...)
	c := New(Options{
		St: st, IsLeader: func() bool { return true },
		Alloc: drbd.NewAllocator(st, drbd.DefaultMinors, drbd.DefaultPorts),
	})
	return c, st
}

func request(t *testing.T, st store.Store, name string, replication int32) {
	t.Helper()
	raw, err := pbproto.Marshal(&pb.VolumeSpec{Name: name, SizeBytes: 64 << 20, Replication: replication})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(context.Background(), storage.PendingCreateKey(name), raw); err != nil {
		t.Fatal(err)
	}
}

func meshed(t *testing.T, c *Controller) map[string]bool {
	t.Helper()
	m, err := c.meshedNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func placed(t *testing.T, st store.Store) []string {
	t.Helper()
	ids, err := storage.ListVolumeIDs(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func pendingLeft(t *testing.T, st store.Store) int {
	t.Helper()
	es, err := st.List(context.Background(), storage.PendingPrefix)
	if err != nil {
		t.Fatal(err)
	}
	return len(es)
}

func TestPendingCreateBecomesASpecStatusAndAllocation(t *testing.T) {
	ctx := context.Background()
	c, st := newPlacer(t, "n1", "n2", "n3")
	request(t, st, "data", 3)
	c.processPending(ctx, meshed(t, c))

	ids := placed(t, st)
	if len(ids) != 1 {
		t.Fatalf("placed %v", ids)
	}
	spec, err := storage.LoadSpec(ctx, st, ids[0])
	if err != nil || spec.Name != "data" || spec.SizeBytes != 64<<20 || spec.Replication != 3 {
		t.Fatalf("spec = %+v, %v", spec, err)
	}
	status, _, err := storage.LoadStatus(ctx, st, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if status.Primary != "n1" {
		t.Errorf("primary = %q, want the first selected node n1", status.Primary)
	}
	if status.State != storage.StateCreating || len(status.Placement) != 3 || !slices.ContainsFunc(status.Placement, func(r storage.Replica) bool { return r.NodeID == status.Primary }) {
		t.Errorf("status = %+v", status)
	}
	for _, r := range status.Placement {
		if r.Healthy || r.Role != "" {
			t.Errorf("row %+v claims a state nobody has observed", r)
		}
	}
	al, err := c.opts.Alloc.Get(ctx, ids[0])
	if err != nil || len(al.NodeIDs) != 3 {
		t.Fatalf("allocation = %+v, %v", al, err)
	}
	if pendingLeft(t, st) != 0 {
		t.Error("request not consumed")
	}
}

func TestPendingCreateNeedsEnoughLiveNodesAndIsKept(t *testing.T) {
	c, st := newPlacer(t, "n1", "n2")
	request(t, st, "data", 3)
	c.processPending(context.Background(), meshed(t, c))
	if len(placed(t, st)) != 0 {
		t.Error("placed on too few nodes")
	}
	if pendingLeft(t, st) != 1 {
		t.Error("an unplaceable request must stay queued for when nodes join")
	}
}

func TestPendingCreateSkipsWitnesses(t *testing.T) {
	ctx := context.Background()
	c, st := newPlacer(t, "n1", "n2", "w1")
	_, _ = st.Put(ctx, "/nodes/w1", []byte(`{"role":"witness"}`))
	request(t, st, "data", 3)
	c.processPending(ctx, meshed(t, c))
	if len(placed(t, st)) != 0 {
		t.Error("a witness holds no storage but was counted as a replica node")
	}
}

func TestPendingCreateSkipsDeadNodes(t *testing.T) {
	c, st := newPlacer(t, "n1", "n2", "n3")
	request(t, st, "data", 3)
	m := meshed(t, c)
	m["n3"] = false
	c.processPending(context.Background(), m)
	if len(placed(t, st)) != 0 {
		t.Error("placed on a dead node")
	}
}

func TestPendingCreateDefaultsTheReplicationFactor(t *testing.T) {
	ctx := context.Background()
	c, st := newPlacer(t, "n1", "n2", "n3")
	request(t, st, "data", 0)
	c.processPending(ctx, meshed(t, c))
	spec, err := storage.LoadSpec(ctx, st, placed(t, st)[0])
	if err != nil || spec.Replication != storage.DefaultStorageClass().Replication {
		t.Errorf("replication = %d, %v", spec.Replication, err)
	}
}

func TestPendingCreateOfAnExistingNameIsDropped(t *testing.T) {
	ctx := context.Background()
	c, st := newPlacer(t, "n1", "n2", "n3")
	request(t, st, "data", 3)
	c.processPending(ctx, meshed(t, c))
	request(t, st, "data", 3)
	c.processPending(ctx, meshed(t, c))
	if n := len(placed(t, st)); n != 1 {
		t.Errorf("%d volumes named data", n)
	}
	if pendingLeft(t, st) != 0 {
		t.Error("a duplicate request must not wedge the queue")
	}
}

func TestPendingCreateResumesAfterACrashBeforeTheSpecWasWritten(t *testing.T) {
	ctx := context.Background()
	c, st := newPlacer(t, "n1", "n2", "n3", "n4")
	request(t, st, "data", 2)
	e, _ := st.Get(ctx, storage.PendingCreateKey("data"))
	id := volumeID("data", e.Revision)
	al, _ := c.opts.Alloc.Allocate(ctx, id)
	for _, h := range []string{"n3", "n4"} { // the crashed attempt had chosen these
		if _, err := c.opts.Alloc.AssignNodeID(ctx, al.Name, h); err != nil {
			t.Fatal(err)
		}
	}
	c.processPending(ctx, meshed(t, c))
	status, _, err := storage.LoadStatus(ctx, st, id)
	if err != nil {
		t.Fatal(err)
	}
	var hosts []string
	for _, r := range status.Placement {
		hosts = append(hosts, r.NodeID)
	}
	slices.Sort(hosts)
	if !slices.Equal(hosts, []string{"n3", "n4"}) {
		t.Errorf("placed on %v: node-ids were already handed to n3 and n4", hosts)
	}
}

func TestUndecodableRequestIsDropped(t *testing.T) {
	c, st := newPlacer(t, "n1", "n2", "n3")
	_, _ = st.Put(context.Background(), storage.PendingCreateKey("junk"), []byte{0xff, 0xff})
	c.processPending(context.Background(), meshed(t, c))
	if pendingLeft(t, st) != 0 {
		t.Error("junk wedged the queue")
	}
}

func TestVolumeIDDiffersForARecreatedName(t *testing.T) {
	if volumeID("data", 7) == volumeID("data", 8) {
		t.Error("a recreated volume reused the id of the deleted one, and so its stale LVs")
	}
	if first, again := volumeID("data", 7), volumeID("data", 7); first != again {
		t.Error("id not stable across retries of one request")
	}
}

func TestRequestWithoutANameIsDropped(t *testing.T) {
	c, st := newPlacer(t, "n1", "n2", "n3")
	raw, _ := pbproto.Marshal(&pb.VolumeSpec{SizeBytes: 1 << 20})
	_, _ = st.Put(context.Background(), storage.PendingCreateKey("anon"), raw)
	c.processPending(context.Background(), meshed(t, c))
	if pendingLeft(t, st) != 0 || len(placed(t, st)) != 0 {
		t.Error("a nameless request was placed or wedged the queue")
	}
}

// failingStatus refuses to write any volume status.
type failingStatus struct{ store.Store }

func (s failingStatus) Put(ctx context.Context, k store.Key, v []byte) (store.Revision, error) {
	if strings.HasSuffix(string(k), "/status") {
		return 0, errors.New("disk full")
	}
	return s.Store.Put(ctx, k, v)
}

func TestNoSpecIsVisibleUntilTheStatusIsWritten(t *testing.T) {
	c, st := newPlacer(t, "n1", "n2", "n3")
	request(t, st, "data", 3)
	c.opts.St = failingStatus{st}
	c.processPending(context.Background(), meshed(t, c))
	if len(placed(t, st)) != 0 {
		t.Error("nodes could discover a volume whose status was never written")
	}
	if pendingLeft(t, st) != 1 {
		t.Error("request dropped although placement failed")
	}
}
