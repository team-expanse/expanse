package volume

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/cluster/lease"
	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/storage/drbd"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/boltstore"
)

// trace is a call log shared by the fakes, safe for the goroutine that runs Hold.
type trace struct {
	mu    sync.Mutex
	calls []string
}

func (t *trace) add(call string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls = append(t.calls, call)
}

func (t *trace) all() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.calls...)
}

func (t *trace) count(call string) (n int) {
	for _, c := range t.all() {
		if c == call {
			n++
		}
	}
	return n
}

// gateDRBD models the role and quorum of one resource; unexpected calls panic.
type gateDRBD struct {
	drbd.DRBD
	tr *trace

	mu             sync.Mutex
	st             drbd.Status
	missing        bool
	promoteFails   int // Primary refuses this many times
	secondaryFails int // Secondary refuses this many times (device held open)
}

func (g *gateDRBD) set(fn func(*drbd.Status)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	fn(&g.st)
}

func (g *gateDRBD) Status(context.Context, string) (*drbd.Status, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.missing {
		return nil, experrors.New(experrors.KindNotFound, "fake", "not configured")
	}
	cp := g.st
	return &cp, nil
}

// Primary refuses like DRBD does: without an up-to-date disk, or while told to.
func (g *gateDRBD) Primary(context.Context, string) error {
	g.tr.add("primary")
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.st.Volumes[0].DiskState == drbd.DiskInconsistent {
		return errors.New("refused: no up-to-date data")
	}
	if g.promoteFails > 0 {
		g.promoteFails--
		return errors.New("refused: peer is primary")
	}
	g.st.Role = drbd.RolePrimary
	return nil
}

func (g *gateDRBD) ForcePrimary(context.Context, string) error {
	g.tr.add("force-primary")
	g.set(func(s *drbd.Status) { s.Role = drbd.RolePrimary })
	return nil
}

func (g *gateDRBD) Secondary(context.Context, string) error {
	g.tr.add("secondary")
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.secondaryFails > 0 {
		g.secondaryFails--
		return errors.New("device is held open by someone")
	}
	g.st.Role = drbd.RoleSecondary
	return nil
}

type gateConsumer struct {
	tr *trace

	mu    sync.Mutex
	fails int // Release fails this many times
}

func (c *gateConsumer) Release(context.Context, string) error {
	c.tr.add("release")
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fails > 0 {
		c.fails--
		return errors.New("umount: target is busy")
	}
	return nil
}

type fakeLease struct {
	done chan struct{}
	once sync.Once
}

func newFakeLease() *fakeLease { return &fakeLease{done: make(chan struct{})} }

func (l *fakeLease) Done() <-chan struct{} { return l.done }
func (l *fakeLease) lose()                 { l.once.Do(func() { close(l.done) }) }
func (l *fakeLease) Valid() bool {
	select {
	case <-l.done:
		return false
	default:
		return true
	}
}

func status(role drbd.Role, quorum bool, disk drbd.DiskState, peers ...drbd.Peer) drbd.Status {
	return drbd.Status{
		Name: "vol-a1", Role: role, Peers: peers,
		Volumes: []drbd.Volume{{DiskState: disk, Quorum: quorum}},
	}
}

func peer(conn drbd.Connection, disk drbd.DiskState) drbd.Peer {
	return drbd.Peer{Connection: conn, Volumes: []drbd.PeerVolume{{DiskState: disk}}}
}

func TestDecideTruthTable(t *testing.T) {
	cases := []struct {
		name          string
		role          drbd.Role
		quorum, lease bool
		want          verb
	}{
		{"lease and quorum, secondary: promote", drbd.RoleSecondary, true, true, promote},
		{"lease and quorum, primary: nothing to do", drbd.RolePrimary, true, true, wait},
		{"lease without quorum, secondary: wait for quorum", drbd.RoleSecondary, false, true, wait},
		{"lease without quorum, primary: stay, DRBD errors the I/O", drbd.RolePrimary, false, true, wait},
		{"quorum without lease, secondary: stay secondary", drbd.RoleSecondary, true, false, wait},
		{"quorum without lease, primary: demote", drbd.RolePrimary, true, false, demote},
		{"neither, secondary: stay secondary", drbd.RoleSecondary, false, false, wait},
		{"neither, primary: demote", drbd.RolePrimary, false, false, demote},
		{"unknown role with lease: do nothing", drbd.RoleUnknown, true, true, wait},
		{"unknown role without lease: do nothing", drbd.RoleUnknown, true, false, wait},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := status(c.role, c.quorum, drbd.DiskUpToDate)
			if got := decide(&st, c.lease, false); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestDecideForcesOnlyAFreshResourceThatNobodyHoldsDataFor(t *testing.T) {
	inc, conn := drbd.DiskInconsistent, drbd.ConnConnected
	cases := []struct {
		name    string
		initial bool
		disk    drbd.DiskState
		peers   []drbd.Peer
		want    verb
	}{
		{"all inconsistent and connected", true, inc, []drbd.Peer{peer(conn, inc), peer(conn, inc)}, forcePromote},
		{"single replica, no peers", true, inc, nil, forcePromote},
		{"not marked initial: plain promote, DRBD decides", false, inc, []drbd.Peer{peer(conn, inc)}, promote},
		{"a peer is unreachable: it may hold data, so no force", true, inc, []drbd.Peer{peer(conn, inc), peer(drbd.ConnConnecting, inc)}, promote},
		{"a peer is standalone", true, inc, []drbd.Peer{peer(drbd.ConnStandAlone, inc)}, promote},
		{"a peer has up-to-date data", true, inc, []drbd.Peer{peer(conn, drbd.DiskUpToDate)}, promote},
		{"our own disk is up to date: plain promote", true, drbd.DiskUpToDate, []drbd.Peer{peer(conn, inc)}, promote},
		{"a peer's disk state is unknown", true, inc, []drbd.Peer{peer(conn, drbd.DiskUnknown)}, promote},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := status(drbd.RoleSecondary, true, c.disk, c.peers...)
			if got := decide(&st, true, c.initial); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestDecideWaitsOnStatusThatReportsNoVolumes(t *testing.T) {
	st := drbd.Status{Role: drbd.RoleSecondary}
	if got := decide(&st, true, true); got != wait {
		t.Errorf("got %v; quorum over zero volumes must not count as quorum", got)
	}
	st = status(drbd.RoleSecondary, true, drbd.DiskInconsistent, drbd.Peer{Connection: drbd.ConnConnected})
	if got := decide(&st, true, true); got != promote {
		t.Errorf("got %v; a connected peer that reports no volumes is not proof of a fresh resource", got)
	}
}

func TestDecideNeverForcesWithoutTheLeaseOrQuorum(t *testing.T) {
	st := status(drbd.RoleSecondary, true, drbd.DiskInconsistent)
	if got := decide(&st, false, true); got != wait {
		t.Errorf("no lease: got %v", got)
	}
	st = status(drbd.RoleSecondary, false, drbd.DiskInconsistent)
	if got := decide(&st, true, true); got != wait {
		t.Errorf("no quorum: got %v", got)
	}
}

// gate is one Promoter wired to fakes; Hold runs in the background.
type gate struct {
	drbd  *gateDRBD
	cons  *gateConsumer
	lease *fakeLease
	p     *Promoter
	tr    *trace
	done  chan error
}

func newGate(st drbd.Status) *gate {
	tr := &trace{}
	d := &gateDRBD{tr: tr, st: st}
	c := &gateConsumer{tr: tr}
	return &gate{
		drbd: d, cons: c, lease: newFakeLease(), tr: tr,
		p: &Promoter{DRBD: d, Consumer: c, Poll: time.Millisecond, Settled: time.Millisecond, StepDownTimeout: time.Second},
	}
}

func (g *gate) hold(ctx context.Context, opt HoldOptions) {
	g.done = make(chan error, 1)
	go func() { g.done <- g.p.Hold(ctx, "vol-a1", g.lease, opt) }()
}

func (g *gate) role() drbd.Role {
	st, _ := g.drbd.Status(context.Background(), "vol-a1")
	return st.Role
}

func (g *gate) finished(t *testing.T) error {
	t.Helper()
	select {
	case err := <-g.done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("Hold did not return")
		return nil
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestHoldPromotesWhenLeaseAndQuorumAreBothThere(t *testing.T) {
	g := newGate(status(drbd.RoleSecondary, true, drbd.DiskUpToDate))
	g.hold(context.Background(), HoldOptions{})
	waitFor(t, "promotion", func() bool { return g.role() == drbd.RolePrimary })
	time.Sleep(20 * time.Millisecond)
	if n := g.tr.count("primary"); n != 1 {
		t.Errorf("promoted %d times; a primary must be left alone", n)
	}
	g.lease.lose()
	g.finished(t)
}

func TestHoldWaitsForQuorumBeforePromoting(t *testing.T) {
	g := newGate(status(drbd.RoleSecondary, false, drbd.DiskUpToDate))
	g.hold(context.Background(), HoldOptions{})
	time.Sleep(30 * time.Millisecond)
	if n := g.tr.count("primary"); n != 0 {
		t.Fatalf("promoted %d times without quorum", n)
	}
	g.drbd.set(func(s *drbd.Status) { s.Volumes[0].Quorum = true })
	waitFor(t, "promotion after quorum returns", func() bool { return g.role() == drbd.RolePrimary })
	g.lease.lose()
	g.finished(t)
}

func TestHoldNeverPromotesOnALeaseThatIsAlreadyGone(t *testing.T) {
	g := newGate(status(drbd.RoleSecondary, true, drbd.DiskUpToDate))
	g.lease.lose()
	g.hold(context.Background(), HoldOptions{})
	if err := g.finished(t); err != nil {
		t.Fatal(err)
	}
	if n := g.tr.count("primary") + g.tr.count("force-primary"); n != 0 {
		t.Errorf("promoted %d times on a lost lease", n)
	}
}

func TestHoldKeepsRetryingARefusedPromotion(t *testing.T) {
	g := newGate(status(drbd.RoleSecondary, true, drbd.DiskUpToDate))
	g.drbd.promoteFails = 3 // the old primary has not stepped down yet
	g.hold(context.Background(), HoldOptions{})
	waitFor(t, "promotion after refusals", func() bool { return g.role() == drbd.RolePrimary })
	if n := g.tr.count("primary"); n != 4 {
		t.Errorf("got %d promotion attempts, want 4", n)
	}
	g.lease.lose()
	g.finished(t)
}

func TestHoldReportsEachPromotion(t *testing.T) {
	g := newGate(status(drbd.RoleSecondary, true, drbd.DiskUpToDate))
	var mu sync.Mutex
	got := 0
	g.hold(context.Background(), HoldOptions{OnPrimary: func() { mu.Lock(); got++; mu.Unlock() }})
	waitFor(t, "promotion", func() bool { return g.role() == drbd.RolePrimary })
	waitFor(t, "OnPrimary", func() bool { mu.Lock(); defer mu.Unlock(); return got == 1 })
	g.lease.lose()
	g.finished(t)
}

func TestHoldForcesTheInitialPromotionOnlyWhenTheGuardHolds(t *testing.T) {
	inc, conn := drbd.DiskInconsistent, drbd.ConnConnected
	g := newGate(status(drbd.RoleSecondary, true, inc, peer(conn, inc), peer(drbd.ConnConnecting, inc)))
	g.hold(context.Background(), HoldOptions{Initial: true})
	time.Sleep(30 * time.Millisecond)
	if g.tr.count("force-primary") != 0 || g.role() != drbd.RoleSecondary {
		t.Fatal("forced a promotion while a peer was unreachable")
	}
	g.drbd.set(func(s *drbd.Status) { s.Peers[1].Connection = conn })
	waitFor(t, "forced promotion once every peer is connected", func() bool { return g.role() == drbd.RolePrimary })
	if g.tr.count("force-primary") != 1 {
		t.Errorf("calls: %v", g.tr.all())
	}
	g.lease.lose()
	g.finished(t)
}

func TestLeaseLossReleasesTheConsumerThenDemotes(t *testing.T) {
	g := newGate(status(drbd.RolePrimary, true, drbd.DiskUpToDate))
	g.hold(context.Background(), HoldOptions{})
	time.Sleep(10 * time.Millisecond)
	g.lease.lose()
	if err := g.finished(t); err != nil {
		t.Fatal(err)
	}
	want := []string{"release", "secondary"}
	if got := g.tr.all(); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if g.role() != drbd.RoleSecondary {
		t.Error("still primary after the lease was lost")
	}
}

func TestEveryWayOfLeavingStepsDown(t *testing.T) {
	t.Run("shutdown cancels a context that is already dead", func(t *testing.T) {
		g := newGate(status(drbd.RolePrimary, true, drbd.DiskUpToDate))
		ctx, cancel := context.WithCancel(context.Background())
		g.hold(ctx, HoldOptions{})
		time.Sleep(10 * time.Millisecond)
		cancel()
		if err := g.finished(t); err != nil {
			t.Fatal(err)
		}
		if g.role() != drbd.RoleSecondary || g.tr.count("release") == 0 {
			t.Errorf("not stepped down: role %s, calls %v", g.role(), g.tr.all())
		}
	})
	t.Run("cancelled before it ever ran", func(t *testing.T) {
		g := newGate(status(drbd.RolePrimary, true, drbd.DiskUpToDate)) // a primary the agent inherited
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		g.hold(ctx, HoldOptions{})
		g.finished(t)
		if g.role() != drbd.RoleSecondary {
			t.Error("an inherited primary was left in place")
		}
	})
	t.Run("a secondary still gets its consumer released", func(t *testing.T) {
		g := newGate(status(drbd.RoleSecondary, false, drbd.DiskUpToDate))
		g.hold(context.Background(), HoldOptions{})
		time.Sleep(10 * time.Millisecond)
		g.lease.lose()
		g.finished(t)
		if g.tr.count("release") == 0 {
			t.Error("a device could be left attached on a non-primary")
		}
		if g.tr.count("secondary") != 0 {
			t.Error("demoted something that was not primary")
		}
	})
	t.Run("a resource that is already gone needs no demotion", func(t *testing.T) {
		g := newGate(status(drbd.RolePrimary, true, drbd.DiskUpToDate))
		g.drbd.missing = true
		g.lease.lose()
		g.hold(context.Background(), HoldOptions{})
		if err := g.finished(t); err != nil {
			t.Fatal(err)
		}
		if g.tr.count("release") == 0 || g.tr.count("secondary") != 0 {
			t.Errorf("calls: %v", g.tr.all())
		}
	})
}

func TestStepDownRetriesWhileTheDeviceIsBusy(t *testing.T) {
	g := newGate(status(drbd.RolePrimary, true, drbd.DiskUpToDate))
	g.drbd.secondaryFails = 3
	g.cons.fails = 2
	g.hold(context.Background(), HoldOptions{})
	time.Sleep(10 * time.Millisecond)
	g.lease.lose()
	if err := g.finished(t); err != nil {
		t.Fatalf("a busy device that frees up must still end in success: %v", err)
	}
	if g.role() != drbd.RoleSecondary {
		t.Error("never demoted")
	}
	if g.tr.count("release") < 3 {
		t.Errorf("release ran %d times; it must be retried until it succeeds", g.tr.count("release"))
	}
}

func TestStepDownThatCannotFinishIsAnError(t *testing.T) {
	g := newGate(status(drbd.RolePrimary, true, drbd.DiskUpToDate))
	g.p.StepDownTimeout = 30 * time.Millisecond
	g.drbd.secondaryFails = 1 << 30
	g.hold(context.Background(), HoldOptions{})
	time.Sleep(10 * time.Millisecond)
	g.lease.lose()
	err := g.finished(t)
	if err == nil {
		t.Fatal("a device that could not be demoted must be reported")
	}
	if g.role() != drbd.RolePrimary {
		t.Error("fake demoted despite refusing")
	}
}

func TestSecondaryIsAttemptedEvenWhenReleaseFails(t *testing.T) {
	g := newGate(status(drbd.RolePrimary, true, drbd.DiskUpToDate))
	g.cons.fails = 1 << 30 // an unmount that errors although nothing is mounted
	g.p.StepDownTimeout = 30 * time.Millisecond
	g.hold(context.Background(), HoldOptions{})
	time.Sleep(10 * time.Millisecond)
	g.lease.lose()
	if err := g.finished(t); err == nil {
		t.Error("a failed release must be reported")
	}
	if g.role() != drbd.RoleSecondary {
		t.Error("DRBD is the judge of whether the device is open; the demotion must still be tried")
	}
}

func TestLossOfQuorumWhilePrimaryDoesNotDemote(t *testing.T) {
	g := newGate(status(drbd.RolePrimary, true, drbd.DiskUpToDate))
	g.hold(context.Background(), HoldOptions{})
	time.Sleep(10 * time.Millisecond)
	g.drbd.set(func(s *drbd.Status) { s.Volumes[0].Quorum = false })
	time.Sleep(30 * time.Millisecond)
	if g.tr.count("secondary") != 0 {
		t.Error("demoted on quorum loss; DRBD's io-error policy already fails the I/O")
	}
	g.lease.lose()
	g.finished(t)
}

// The real lease, revoked from outside, must demote the device.
func TestRealLeaseRevocationDemotes(t *testing.T) {
	st, err := boltstore.New(filepath.Join(t.TempDir(), "lease.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mgr := lease.NewManager(st, "n1")
	held, err := mgr.Acquire(context.Background(), "storage-primary:vol-a1", 600*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	g := newGate(status(drbd.RoleSecondary, true, drbd.DiskUpToDate))
	done := make(chan error, 1)
	go func() { done <- g.p.Hold(context.Background(), "vol-a1", held, HoldOptions{}) }()
	waitFor(t, "promotion", func() bool { return g.role() == drbd.RolePrimary })

	rev := held.CurrentLease().Revision
	if err := st.Delete(context.Background(), store.Key(lease.Prefix+"storage-primary:vol-a1"), rev); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Hold did not return after the lease was revoked")
	}
	if g.role() != drbd.RoleSecondary {
		t.Error("lease revoked but the volume is still primary")
	}
}

// leaderRig runs Lead for node n1 against a shared store, with a second manager
// standing in for the other nodes.
type leaderRig struct {
	*gate
	st     *boltstore.Store
	mine   *lease.Manager
	other  *lease.Manager
	cancel context.CancelFunc
	ended  chan error
	ttl    time.Duration
}

const leaseName = "storage-primary:vol-a1"

func newLeaderRig(t *testing.T, st drbd.Status) *leaderRig {
	t.Helper()
	bolt, err := boltstore.New(filepath.Join(t.TempDir(), "lease.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bolt.Close() })
	return &leaderRig{
		gate: newGate(st), st: bolt,
		mine: lease.NewManager(bolt, "n1"), other: lease.NewManager(bolt, "n2"),
		ttl: 600 * time.Millisecond,
	}
}

func (r *leaderRig) lead(opt HoldOptions) {
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.ended = make(chan error, 1)
	go func() { r.ended <- r.p.Lead(ctx, r.mine, leaseName, "vol-a1", r.ttl, opt) }()
}

func (r *leaderRig) stop(t *testing.T) error {
	t.Helper()
	r.cancel()
	select {
	case err := <-r.ended:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("Lead did not return")
		return nil
	}
}

func (r *leaderRig) holder(t *testing.T) string {
	t.Helper()
	h, err := r.other.Holder(context.Background(), leaseName)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestLeadPromotesUnderTheLeaseAndGivesItUpOnceDemoted(t *testing.T) {
	r := newLeaderRig(t, status(drbd.RoleSecondary, true, drbd.DiskUpToDate))
	r.lead(HoldOptions{})
	waitFor(t, "promotion", func() bool { return r.role() == drbd.RolePrimary })
	if got := r.holder(t); got != "n1" {
		t.Fatalf("lease holder %q while primary", got)
	}
	if err := r.stop(t); err != nil {
		t.Fatal(err)
	}
	if r.role() != drbd.RoleSecondary || r.holder(t) != "" {
		t.Errorf("role %s, holder %q; want Secondary and the lease released", r.role(), r.holder(t))
	}
}

func TestLeadKeepsTheRecordWhenTheDemotionFails(t *testing.T) {
	r := newLeaderRig(t, status(drbd.RoleSecondary, true, drbd.DiskUpToDate))
	r.p.StepDownTimeout = 30 * time.Millisecond
	r.lead(HoldOptions{})
	waitFor(t, "promotion", func() bool { return r.role() == drbd.RolePrimary })
	r.drbd.mu.Lock()
	r.drbd.secondaryFails = 1 << 30
	r.drbd.mu.Unlock()
	if err := r.stop(t); err == nil {
		t.Fatal("a demotion that failed must be reported")
	}
	if _, err := r.other.TryAcquire(context.Background(), leaseName, time.Second); err == nil {
		t.Fatal("another node was admitted while this one may still be primary")
	}
}

func TestLeadTakesTheLeaseBackAfterItIsRevoked(t *testing.T) {
	r := newLeaderRig(t, status(drbd.RoleSecondary, true, drbd.DiskUpToDate))
	r.lead(HoldOptions{})
	t.Cleanup(func() { r.stop(t) })
	waitFor(t, "first promotion", func() bool { return r.role() == drbd.RolePrimary })

	if err := r.st.Delete(context.Background(), store.Key(lease.Prefix+leaseName), 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "demotion", func() bool { return r.tr.count("secondary") >= 1 })
	waitFor(t, "second promotion", func() bool { return r.tr.count("primary") >= 2 && r.role() == drbd.RolePrimary })
}

func TestLeadDemotesAnInheritedPrimaryWhileWaitingForTheLease(t *testing.T) {
	r := newLeaderRig(t, status(drbd.RolePrimary, true, drbd.DiskUpToDate)) // left over from a crashed agent
	other, err := r.other.TryAcquire(context.Background(), leaseName, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	r.lead(HoldOptions{})
	waitFor(t, "demotion of the inherited primary", func() bool { return r.role() == drbd.RoleSecondary })
	time.Sleep(30 * time.Millisecond)
	if r.tr.count("primary") != 0 {
		t.Fatal("promoted without the lease")
	}
	if err := r.other.Release(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "promotion once the lease is free", func() bool { return r.role() == drbd.RolePrimary })
	if err := r.stop(t); err != nil {
		t.Fatal(err)
	}
}

func TestLeadStopsCleanlyWhileStillWaiting(t *testing.T) {
	r := newLeaderRig(t, status(drbd.RoleSecondary, true, drbd.DiskUpToDate))
	if _, err := r.other.TryAcquire(context.Background(), leaseName, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	r.lead(HoldOptions{})
	time.Sleep(20 * time.Millisecond)
	if err := r.stop(t); err != nil {
		t.Fatalf("cancelling a leader that never held the lease is not an error: %v", err)
	}
	if r.tr.count("primary") != 0 {
		t.Error("promoted without the lease")
	}
}

func TestLeadResumesAtOnceOnItsOwnLiveRecord(t *testing.T) {
	r := newLeaderRig(t, status(drbd.RoleSecondary, true, drbd.DiskUpToDate))
	r.ttl = time.Minute
	old, err := r.mine.TryAcquire(context.Background(), leaseName, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	old.Abandon() // the previous agent died holding it
	r.lead(HoldOptions{})
	waitFor(t, "promotion without waiting out the old record", func() bool { return r.role() == drbd.RolePrimary })
	if err := r.stop(t); err != nil {
		t.Fatal(err)
	}
}
