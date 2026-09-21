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

	verified, resynced []string
	checkErr           error

	rejoined  []bool // the discard flag of each Rejoin
	rejoinErr error
	onRejoin  func()
}

func (f *fakeConverger) Rejoin(_ context.Context, _ Desired, discard bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.onRejoin != nil {
		f.onRejoin()
	}
	if f.rejoinErr != nil {
		return f.rejoinErr
	}
	f.rejoined = append(f.rejoined, discard)
	return nil
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
	marks SplitBrainMarks
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
	marks, err := NewSplitBrainMarks(filepath.Join(t.TempDir(), "split-brain"))
	if err != nil {
		t.Fatal(err)
	}
	r.marks = marks
	r.node = &Node{
		Splits: marks, Self: "n1", St: st, Alloc: r.alloc, RT: r.conv, DRBD: r.drbd, Lead: r.lead.lead,
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
}

// Adjusting a resource the kernel dropped would reconnect it and start the split-brain over.
func TestADivergedVolumeIsLeftAsTheKernelDroppedIt(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n1", "n1", "n2")
	r.setState(t, storage.StateNeedsManualRecovery)
	r.mustSync(t)
	if len(r.conv.desired) != 0 {
		t.Errorf("reconciled a diverged volume: %d passes", len(r.conv.desired))
	}
}

func TestADivergedVolumeStillReportsItsReplica(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n1", "n1", "n2")
	r.drbd.st[vol] = fixtureStatus(t, "healthy-secondary.n2.json")
	r.node.Self = "n2"
	r.setState(t, storage.StateNeedsManualRecovery)
	r.mustSync(t)
	if row, _ := r.row(t, "n2"); row.LastSeen.IsZero() {
		t.Errorf("row not published: %+v", row)
	}
}

func TestAKernelSplitBrainMarkMovesTheVolumeToManualRecovery(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n1", "n1", "n2")
	r.mustSync(t)
	waitStarted(t, r.lead)
	runHandler(t, r.marks, vol)
	r.mustSync(t)
	if got := r.status(t).State; got != storage.StateNeedsManualRecovery {
		t.Errorf("state = %s", got)
	}
	if r.lead.isRunning(vol) {
		t.Error("the primary kept leading a diverged volume")
	}
}

func TestASplitBrainMarkDoesNotOverwriteADeletingVolume(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n1", "n1", "n2")
	r.setState(t, storage.StateDeleting)
	runHandler(t, r.marks, vol)
	r.mustSync(t)
	if got := r.status(t).State; got != storage.StateDeleting {
		t.Errorf("state = %s", got)
	}
}

func TestDeletingAVolumeForgetsItsSplitBrainMark(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n1", "n1", "n2")
	r.mustSync(t)
	runHandler(t, r.marks, vol)
	r.setState(t, storage.StateDeleting)
	r.mustSync(t)
	if got, _ := r.marks.Marked(vol); got {
		t.Error("the mark outlived the volume")
	}
}

func TestAMarkOfAnotherVolumeChangesNothing(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n1", "n1", "n2")
	runHandler(t, r.marks, "vol-other")
	r.mustSync(t)
	if got := r.status(t).State; got != storage.StateHealthy {
		t.Errorf("state = %s", got)
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

// deletingStore marks the volume Deleting just before the first write it is asked to make.
type deletingStore struct {
	store.Store
	once sync.Once
}

func (s *deletingStore) CompareAndSwap(ctx context.Context, k store.Key, rev store.Revision, v []byte) (store.Revision, error) {
	s.once.Do(func() {
		st, _, err := storage.LoadStatus(ctx, s.Store, vol)
		if err == nil {
			st.State = storage.StateDeleting
			_ = storage.SaveStatus(ctx, s.Store, vol, st)
		}
	})
	return s.Store.CompareAndSwap(ctx, k, rev, v)
}

func TestASplitBrainMarkLosesToADeleteRequestedMeanwhile(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n1", "n1", "n2")
	runHandler(t, r.marks, vol)
	r.node.St = &deletingStore{Store: r.st}
	r.mustSync(t)
	if got := r.status(t).State; got != storage.StateDeleting {
		t.Errorf("state = %s: the mark overwrote the delete", got)
	}
}

// peerVolume is the first volume of the peer with the given DRBD node-id.
func peerVolume(t *testing.T, st *drbd.Status, id int) *drbd.PeerVolume {
	t.Helper()
	for i := range st.Peers {
		if st.Peers[i].NodeID == id {
			return &st.Peers[i].Volumes[0]
		}
	}
	t.Fatalf("no peer %d in %+v", id, st.Peers)
	return nil
}

func TestSyncPublishesTheResyncProgressOfThisReplica(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n1", "n1", "n2", "n3")
	r.drbd.st[vol] = fixtureStatus(t, "syncing-target.n3.json")
	r.node.Self = "n3"
	r.mustSync(t)
	if row, _ := r.row(t, "n3"); row.Role != storage.RoleResyncing || row.SyncPercent != 80 {
		t.Errorf("row = %+v, want Resyncing at 80%% (the kernel says 80.17)", row)
	}
}

func TestSyncWritesProgressOnlyWhenAWholePercentPasses(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n1", "n1", "n2", "n3")
	r.drbd.st[vol] = fixtureStatus(t, "syncing-target.n3.json")
	r.node.Self = "n3"
	r.mustSync(t)
	before := revisionOf(t, r)
	for _, peer := range r.drbd.st[vol].Peers {
		for i := range peer.Volumes {
			peer.Volumes[i].PercentInSync = 80.9
		}
	}
	r.mustSync(t)
	if after := revisionOf(t, r); after != before {
		t.Errorf("rewritten for a move inside one percent: %d -> %d", before, after)
	}
	for _, peer := range r.drbd.st[vol].Peers {
		for i := range peer.Volumes {
			peer.Volumes[i].PercentInSync = 81.2
		}
	}
	r.mustSync(t)
	if row, _ := r.row(t, "n3"); row.SyncPercent != 81 {
		t.Errorf("row = %+v, want 81%%", row)
	}
}

func TestThePrimaryPublishesWhatAVerifyFoundAgainstEachPeer(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n1", "n1", "n2", "n3")
	st := fixtureStatus(t, "healthy-primary.n1.json")
	peerVolume(t, st, 1).OutOfSyncKiB = 2048
	peerVolume(t, st, 2).Replication = drbd.ReplVerifyS
	r.drbd.st[vol] = st
	r.mustSync(t)
	if row, _ := r.row(t, "n2"); row.OutOfSyncKiB != 2048 || row.Verifying {
		t.Errorf("n2 = %+v, want 2048 KiB out of sync", row)
	}
	if row, _ := r.row(t, "n3"); row.OutOfSyncKiB != 0 || !row.Verifying {
		t.Errorf("n3 = %+v, want a verify in progress", row)
	}
	if row, _ := r.row(t, "n1"); row.OutOfSyncKiB != 0 || row.Verifying {
		t.Errorf("n1 = %+v: the primary is not out of sync with itself", row)
	}
}

func TestASecondaryLeavesThePeersRowsToThePrimary(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n1", "n1", "n2", "n3")
	st := fixtureStatus(t, "healthy-secondary.n2.json")
	peerVolume(t, st, 0).OutOfSyncKiB = 4096
	r.drbd.st[vol] = st
	r.node.Self = "n2"
	r.mustSync(t)
	if row, _ := r.row(t, "n1"); row.OutOfSyncKiB != 0 {
		t.Errorf("n1 = %+v: only the primary counts against its peers", row)
	}
}

func TestThePrimaryKeepsWhatItLastSawOfAPeerItCannotReach(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n1", "n1", "n2", "n3")
	st := fixtureStatus(t, "healthy-primary.n1.json")
	peerVolume(t, st, 1).OutOfSyncKiB = 2048
	r.drbd.st[vol] = st
	r.mustSync(t)
	for i := range st.Peers {
		if st.Peers[i].NodeID == 1 {
			st.Peers[i].Connection = drbd.ConnConnecting
		}
	}
	r.mustSync(t)
	if row, _ := r.row(t, "n2"); row.OutOfSyncKiB != 2048 {
		t.Errorf("n2 = %+v, want the last count kept while it is unreachable", row)
	}
}

func TestThePrimaryFollowsAPeersCountBackToZero(t *testing.T) {
	r := newNodeRig(t)
	r.place(t, "n1", "n1", "n2", "n3")
	st := fixtureStatus(t, "healthy-primary.n1.json")
	peerVolume(t, st, 1).OutOfSyncKiB = 2048
	r.drbd.st[vol] = st
	r.mustSync(t)
	peerVolume(t, st, 1).OutOfSyncKiB = 0
	r.mustSync(t)
	if row, _ := r.row(t, "n2"); row.OutOfSyncKiB != 0 {
		t.Errorf("n2 = %+v, want the count cleared once the kernel's is", row)
	}
}

func TestANewPrimaryClearsWhatItsPredecessorRecordedAboutIt(t *testing.T) {
	for name, left := range map[string]storage.Replica{
		"a count":  {OutOfSyncKiB: 2048},
		"a verify": {Verifying: true},
	} {
		t.Run(name, func(t *testing.T) {
			r := newNodeRig(t)
			r.place(t, "n2", "n1", "n2", "n3")
			if err := r.node.updateStatus(context.Background(), vol, func(s *storage.Status) bool {
				// Only the leftover differs from what the node will report.
				s.Placement[1].Role, s.Placement[1].OutOfSyncKiB, s.Placement[1].Verifying = storage.RolePrimary, left.OutOfSyncKiB, left.Verifying
				return true
			}); err != nil {
				t.Fatal(err)
			}
			r.drbd.st[vol] = fixtureStatus(t, "healthy-secondary.n2.json")
			r.drbd.st[vol].Role = drbd.RolePrimary
			r.node.Self = "n2"
			r.mustSync(t)
			if row, _ := r.row(t, "n2"); row.OutOfSyncKiB != 0 || row.Verifying {
				t.Errorf("n2 = %+v: the primary's own row cannot hold a verify against itself", row)
			}
		})
	}
}
