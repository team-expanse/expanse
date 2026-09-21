package volume

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/storage/drbd"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/boltstore"
)

const vol = "vol-a1"

type fakeConverger struct {
	mu      sync.Mutex
	desired []Desired
	removed []string
	present map[string]bool
	forgot  []int
	err     error

	snapped, restored []string
	snapErr           error
	restoreErr        error
}

func (f *fakeConverger) Reconcile(_ context.Context, d Desired) (Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.desired = append(f.desired, d)
	if f.err != nil {
		return Result{}, f.err
	}
	f.present[d.Name] = true
	return Result{Forgot: f.forgot}, nil
}

func (f *fakeConverger) Remove(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, name)
	delete(f.present, name)
	return nil
}

func (f *fakeConverger) Present(_ context.Context, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.present[name], nil
}

// fakeLeader runs each Lead call until its context ends, like Promoter.Lead.
type fakeLeader struct {
	mu      sync.Mutex
	opts    map[string]HoldOptions
	running map[string]bool
	started chan string
}

func (l *fakeLeader) lead(ctx context.Context, res string, opt HoldOptions) error {
	l.mu.Lock()
	l.opts[res] = opt
	l.running[res] = true
	l.mu.Unlock()
	l.started <- res
	<-ctx.Done()
	l.mu.Lock()
	l.running[res] = false
	l.mu.Unlock()
	return nil
}

func (l *fakeLeader) isRunning(res string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.running[res]
}

// kernel serves canned DRBD statuses by resource name.
type kernel struct {
	drbd.DRBD
	st map[string]*drbd.Status
}

func (k kernel) Status(_ context.Context, res string) (*drbd.Status, error) {
	if s, ok := k.st[res]; ok {
		return s, nil
	}
	return nil, experrors.New(experrors.KindNotFound, "fake", res)
}

type nodeRig struct {
	st    store.Store
	alloc *drbd.Allocator
	node  *Node
	conv  *fakeConverger
	lead  *fakeLeader
	drbd  kernel
}

func fixtureStatus(t *testing.T, name string) *drbd.Status {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "test", "fixtures", "drbd", name))
	if err != nil {
		t.Fatal(err)
	}
	st, err := drbd.ParseStatus(vol, b)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func newNodeRig(t *testing.T) *nodeRig {
	t.Helper()
	st, err := boltstore.New(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	r := &nodeRig{
		st:    st,
		alloc: drbd.NewAllocator(st, drbd.DefaultMinors, drbd.DefaultPorts),
		conv:  &fakeConverger{present: map[string]bool{}},
		lead:  &fakeLeader{opts: map[string]HoldOptions{}, running: map[string]bool{}, started: make(chan string, 8)},
		drbd:  kernel{st: map[string]*drbd.Status{}},
	}
	r.node = &Node{
		Self: "n1", St: st, Alloc: r.alloc, RT: r.conv, DRBD: r.drbd, Lead: r.lead.lead,
		Addr: func(host string) (netip.Addr, error) {
			return netip.MustParseAddr("10.0.0." + host[1:]), nil
		},
		Thin: true,
	}
	t.Cleanup(r.node.Stop)
	return r
}

// place records a volume of the given size on hosts, with allocations, as the leader would.
func (r *nodeRig) place(t *testing.T, primary string, hosts ...string) {
	t.Helper()
	ctx := context.Background()
	if _, err := r.alloc.Allocate(ctx, vol); err != nil {
		t.Fatal(err)
	}
	var rows []storage.Replica
	for _, h := range hosts {
		if _, err := r.alloc.AssignNodeID(ctx, vol, h); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, storage.Replica{NodeID: h, Role: storage.RoleSecondary, Healthy: true})
	}
	if err := storage.SaveSpec(ctx, r.st, storage.Spec{ID: vol, Name: "data", SizeBytes: 64 << 20, Replication: len(hosts)}); err != nil {
		t.Fatal(err)
	}
	if err := storage.SaveStatus(ctx, r.st, vol, storage.Status{State: storage.StateHealthy, Primary: primary, Placement: rows}); err != nil {
		t.Fatal(err)
	}
}

func (r *nodeRig) setState(t *testing.T, s storage.VolumeState) {
	t.Helper()
	st, rev, err := storage.LoadStatus(context.Background(), r.st, vol)
	if err != nil {
		t.Fatal(err)
	}
	st.State = s
	if err := storage.CompareAndSwapStatus(context.Background(), r.st, vol, rev, st); err != nil {
		t.Fatal(err)
	}
}

func (r *nodeRig) setPrimary(t *testing.T, host string) {
	t.Helper()
	st, rev, err := storage.LoadStatus(context.Background(), r.st, vol)
	if err != nil {
		t.Fatal(err)
	}
	st.Primary = host
	if err := storage.CompareAndSwapStatus(context.Background(), r.st, vol, rev, st); err != nil {
		t.Fatal(err)
	}
}

func (r *nodeRig) sync(t *testing.T) error {
	t.Helper()
	return r.node.Sync(context.Background())
}

func (r *nodeRig) mustSync(t *testing.T) {
	t.Helper()
	if err := r.sync(t); err != nil {
		t.Fatalf("Sync: %v", err)
	}
}

func (r *nodeRig) status(t *testing.T) storage.Status {
	t.Helper()
	st, _, err := storage.LoadStatus(context.Background(), r.st, vol)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func (r *nodeRig) row(t *testing.T, host string) (storage.Replica, bool) {
	t.Helper()
	for _, p := range r.status(t).Placement {
		if p.NodeID == host {
			return p, true
		}
	}
	return storage.Replica{}, false
}

func waitStarted(t *testing.T, l *fakeLeader) string {
	t.Helper()
	select {
	case res := <-l.started:
		return res
	case <-time.After(5 * time.Second):
		t.Fatal("no leader started")
		return ""
	}
}

func TestSyncConvergesAPlacedVolumeWithEveryMember(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n2", "n1", "n2", "n3")
	r.mustSync(t)
	if len(r.conv.desired) != 1 {
		t.Fatalf("reconciled %d times", len(r.conv.desired))
	}
	d := r.conv.desired[0]
	if d.Name != vol || d.Self != "n1" || d.SizeBytes != 64<<20 || !d.Thin || len(d.Members) != 3 {
		t.Errorf("desired = %+v", d)
	}
	if d.Members[2].Address != netip.MustParseAddr("10.0.0.3") {
		t.Errorf("member address = %v", d.Members[2].Address)
	}
}

func TestSyncIgnoresAVolumeThatIsNotPlacedHere(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n2", "n2", "n3")
	r.mustSync(t)
	if len(r.conv.desired)+len(r.conv.removed) != 0 {
		t.Errorf("touched a foreign volume: %+v %v", r.conv.desired, r.conv.removed)
	}
}

func TestSyncRemovesAStaleLocalReplicaOfAVolumeMovedAway(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n2", "n2", "n3")
	r.conv.present[vol] = true
	r.mustSync(t)
	if !slices.Equal(r.conv.removed, []string{vol}) {
		t.Errorf("removed %v", r.conv.removed)
	}
}

func TestSyncSkipsAVolumeWithoutAnAllocationYet(t *testing.T) {
	r := newNodeRig(t)
	ctx := context.Background()
	_ = storage.SaveSpec(ctx, r.st, storage.Spec{ID: vol, SizeBytes: 1 << 20})
	_ = storage.SaveStatus(ctx, r.st, vol, storage.Status{Placement: []storage.Replica{{NodeID: "n1"}}})
	r.mustSync(t)
	if len(r.conv.desired) != 0 {
		t.Error("reconciled before the leader allocated")
	}
}

func TestOneVolumesFailureDoesNotStopTheOthers(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n1", "n1", "n2")
	r.node.Addr = func(string) (netip.Addr, error) { return netip.Addr{}, errors.New("no mesh address") }
	if err := r.sync(t); err == nil {
		t.Fatal("expected the address failure to surface")
	}
	r.node.Addr = func(string) (netip.Addr, error) { return netip.MustParseAddr("10.0.0.1"), nil }
	r.mustSync(t)
	if len(r.conv.desired) != 1 {
		t.Errorf("did not recover: %d passes", len(r.conv.desired))
	}
}

func TestSyncAcknowledgesForgottenNodeIDs(t *testing.T) {
	r := newNodeRig(t)
	ctx := context.Background()
	r.place(t, "n2", "n1", "n2", "n3")
	id, err := r.alloc.RetireNode(ctx, vol, "n3")
	if err != nil {
		t.Fatal(err)
	}
	r.conv.forgot = []int{id}
	r.mustSync(t)
	al, err := r.alloc.Get(ctx, vol)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(al.Acks[id], "n1") {
		t.Errorf("forget of %d not acknowledged: %+v", id, al.Acks)
	}
}

func TestSyncStartsLeadingOnceWhileThisNodeIsPrimary(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n1", "n1", "n2", "n3")
	r.mustSync(t)
	if got := waitStarted(t, r.lead); got != vol {
		t.Fatalf("led %q", got)
	}
	r.mustSync(t)
	select {
	case res := <-r.lead.started:
		t.Fatalf("second leader for %s", res)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestFreshVolumeMayBeForcedUntilItHasBeenPrimary(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n1", "n1", "n2")
	r.mustSync(t)
	waitStarted(t, r.lead)
	opt := r.lead.opts[vol]
	if !opt.Initial || opt.OnPrimary == nil {
		t.Fatalf("options = %+v", opt)
	}
	opt.OnPrimary()
	al, _ := r.alloc.Get(context.Background(), vol)
	if !al.Initialized {
		t.Error("promotion was not recorded")
	}
	r.node.Stop()
	r.mustSync(t)
	waitStarted(t, r.lead)
	if r.lead.opts[vol].Initial {
		t.Error("a volume that has been primary is still forceable")
	}
}

func TestSyncStopsLeadingWhenAnotherNodeIsElected(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n1", "n1", "n2")
	r.mustSync(t)
	waitStarted(t, r.lead)
	r.setPrimary(t, "n2")
	r.mustSync(t)
	if r.lead.isRunning(vol) {
		t.Error("still leading after re-election")
	}
}

func TestNoPromotionWhileTheVolumeNeedsManualRecovery(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n1", "n1", "n2")
	r.setState(t, storage.StateNeedsManualRecovery)
	r.mustSync(t)
	select {
	case <-r.lead.started:
		t.Fatal("led a volume that needs manual recovery")
	case <-time.After(50 * time.Millisecond):
	}
	if len(r.conv.desired) != 1 {
		t.Error("the resource should still be kept up for inspection")
	}
}

func TestDeletingStopsLeadingRemovesTheReplicaAndDropsItsRow(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n1", "n1", "n2")
	r.mustSync(t)
	waitStarted(t, r.lead)
	r.setState(t, storage.StateDeleting)
	r.mustSync(t)
	if r.lead.isRunning(vol) {
		t.Error("still leading a deleted volume")
	}
	if !slices.Equal(r.conv.removed, []string{vol}) {
		t.Errorf("removed %v", r.conv.removed)
	}
	if _, ok := r.row(t, "n1"); ok {
		t.Error("own row not dropped: the controller cannot tell this node is done")
	}
	if _, ok := r.row(t, "n2"); !ok {
		t.Error("another node's row was dropped")
	}
}

func TestDeletingKeepsItsRowWhileTheRemovalFails(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n1", "n1", "n2")
	r.conv.present[vol] = true
	r.node.RT = failingRemover{r.conv}
	r.setState(t, storage.StateDeleting)
	if err := r.sync(t); err == nil {
		t.Fatal("expected the removal failure")
	}
	if _, ok := r.row(t, "n1"); !ok {
		t.Error("acknowledged a deletion that did not finish")
	}
}

type failingRemover struct{ *fakeConverger }

func (failingRemover) Remove(context.Context, string) error { return errors.New("device busy") }

func TestSyncPublishesThisNodesObservedRole(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n2", "n1", "n2", "n3")
	r.drbd.st[vol] = fixtureStatus(t, "healthy-secondary.n2.json")
	r.node.Self = "n2"
	r.mustSync(t)
	row, _ := r.row(t, "n2")
	if row.Role != storage.RoleSecondary || !row.Healthy || row.LastSeen.IsZero() {
		t.Errorf("row = %+v", row)
	}
}

func TestSyncPublishesAStaleReplicaAsUnhealthy(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n1", "n1", "n2", "n3")
	r.drbd.st[vol] = fixtureStatus(t, "syncing-target.n3.json")
	r.node.Self = "n3"
	r.mustSync(t)
	row, _ := r.row(t, "n3")
	if row.Role != storage.RoleResyncing || row.Healthy {
		t.Errorf("row = %+v", row)
	}
}

func revisionOf(t *testing.T, r *nodeRig) store.Revision {
	t.Helper()
	_, rev, err := storage.LoadStatus(context.Background(), r.st, vol)
	if err != nil {
		t.Fatal(err)
	}
	return rev
}

func TestSyncDoesNotRewriteAnUnchangedObservation(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n2", "n1", "n2", "n3")
	r.drbd.st[vol] = fixtureStatus(t, "healthy-secondary.n2.json")
	r.node.Self = "n2"
	r.mustSync(t)
	before := revisionOf(t, r)
	r.mustSync(t)
	if after := revisionOf(t, r); after != before {
		t.Errorf("status rewritten with nothing changed: %d -> %d", before, after)
	}
}

func TestSyncPublishesNothingWhileTheResourceIsNotUp(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n2", "n1", "n2")
	before := revisionOf(t, r)
	r.mustSync(t)
	if after := revisionOf(t, r); after != before {
		t.Error("published a role for a resource that is not up")
	}
}

func TestStopEndsEveryLeaderBeforeReturning(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n1", "n1", "n2")
	r.mustSync(t)
	waitStarted(t, r.lead)
	r.node.Stop()
	if r.lead.isRunning(vol) {
		t.Error("leader outlived Stop")
	}
}

func TestSyncStopsLeadingAVolumeWhoseRecordsAreGone(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n1", "n1", "n2")
	r.mustSync(t)
	waitStarted(t, r.lead)
	ctx := context.Background()
	_ = r.st.Delete(ctx, storage.SpecKey(vol), 0)
	_ = r.st.Delete(ctx, storage.StatusKey(vol), 0)
	r.mustSync(t)
	if r.lead.isRunning(vol) {
		t.Error("still leading a volume that no longer exists")
	}
}

func TestSyncPublishesAHealthChangeThatLeavesTheRoleUnchanged(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n1", "n1", "n2", "n3")
	if err := r.node.updateStatus(context.Background(), vol, func(s *storage.Status) bool {
		s.Placement[0].Role = storage.RolePrimary
		return true
	}); err != nil {
		t.Fatal(err)
	}
	r.drbd.st[vol] = fixtureStatus(t, "quorum-lost.n1.json")
	r.mustSync(t)
	row, _ := r.row(t, "n1")
	if row.Role != storage.RolePrimary || row.Healthy {
		t.Errorf("row = %+v, want an unhealthy Primary", row)
	}
}

// racingStore makes the controller write once between the node's read and its swap.
type racingStore struct {
	store.Store
	once sync.Once
}

func (s *racingStore) CompareAndSwap(ctx context.Context, k store.Key, rev store.Revision, v []byte) (store.Revision, error) {
	s.once.Do(func() {
		if e, err := s.Get(ctx, k); err == nil {
			_, _ = s.Put(ctx, k, e.Value)
		}
	})
	return s.Store.CompareAndSwap(ctx, k, rev, v)
}

func TestSyncRetriesAPublishThatLostARaceWithTheController(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n2", "n1", "n2", "n3")
	r.node.St = &racingStore{Store: r.st}
	r.drbd.st[vol] = fixtureStatus(t, "syncing-target.n3.json")
	r.node.Self = "n3"
	r.mustSync(t)
	if row, _ := r.row(t, "n3"); row.Role != storage.RoleResyncing {
		t.Errorf("row = %+v: the publish was lost", row)
	}
}
