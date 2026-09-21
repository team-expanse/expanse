package volume

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/storage/drbd"
	"github.com/expanse/expanse/internal/storage/lvm"
)

const extent = 4 << 20

// journal records every call the fakes receive, in order.
type journal struct{ calls []string }

func (j *journal) add(format string, args ...any) {
	j.calls = append(j.calls, fmt.Sprintf(format, args...))
}

func (j *journal) has(call string) bool {
	for _, c := range j.calls {
		if c == call {
			return true
		}
	}
	return false
}

func (j *journal) index(call string) int {
	for i, c := range j.calls {
		if c == call {
			return i
		}
	}
	return -1
}

// mutating drops the read-only queries, leaving what a pass changed.
func (j *journal) mutating() []string {
	var out []string
	for _, c := range j.calls {
		switch c {
		case "lvm.get", "drbd.status", "drbd.has-md", "drbd.adjust-pending":
		default:
			out = append(out, c)
		}
	}
	return out
}

// fakeLVM embeds the interface so an unexpected call panics.
type fakeLVM struct {
	lvm.LVM
	j              *journal
	lvs            map[string]lvm.LV
	err, removeErr error
}

func roundUp(n uint64) uint64 { return (n + extent - 1) / extent * extent }

func (f *fakeLVM) Get(_ context.Context, vg, name string) (lvm.LV, error) {
	f.j.add("lvm.get")
	if lv, ok := f.lvs[name]; ok {
		return lv, nil
	}
	return lvm.LV{}, experrors.New(experrors.KindNotFound, "fake", name)
}

func (f *fakeLVM) create(kind lvm.LVType, pool, name string, size uint64) error {
	if f.err != nil {
		return f.err
	}
	f.lvs[name] = lvm.LV{Name: name, VG: "vg0", Size: roundUp(size), Type: kind, Pool: pool, Active: true}
	return nil
}

func (f *fakeLVM) CreateThin(_ context.Context, vg, pool, name string, size uint64) error {
	f.j.add("lvm.create-thin %s/%s %s %d", vg, pool, name, size)
	return f.create(lvm.Thin, pool, name, size)
}

func (f *fakeLVM) CreateThick(_ context.Context, vg, name string, size uint64) error {
	f.j.add("lvm.create-thick %s %s %d", vg, name, size)
	return f.create(lvm.Thick, "", name, size)
}

func (f *fakeLVM) Extend(_ context.Context, vg, name string, size uint64) error {
	f.j.add("lvm.extend %s %d", name, size)
	lv := f.lvs[name]
	lv.Size = roundUp(size)
	f.lvs[name] = lv
	return nil
}

// fakeDRBD models just enough kernel state to tell whether a pass changed anything.
type fakeDRBD struct {
	drbd.DRBD
	j                  *journal
	lvm                *fakeLVM
	hasMD, up          bool
	pending            bool
	usableKiB          uint64
	mdErr, forgetErr   error
	createMDErr, upErr error
	downErr            error
}

const mdOverheadKiB = 100

// sync gives the device the LV's size minus its internal metadata, as DRBD does.
func (f *fakeDRBD) sync() { f.usableKiB = f.lvm.lvs["vol-a1"].Size/1024 - mdOverheadKiB }

func (f *fakeDRBD) Status(_ context.Context, res string) (*drbd.Status, error) {
	f.j.add("drbd.status")
	if !f.up {
		return nil, experrors.New(experrors.KindNotFound, "fake", res)
	}
	return &drbd.Status{Name: res, Volumes: []drbd.Volume{{SizeKiB: f.usableKiB}}}, nil
}

func (f *fakeDRBD) HasMetadata(context.Context, string) (bool, error) {
	f.j.add("drbd.has-md")
	return f.hasMD, f.mdErr
}

func (f *fakeDRBD) CreateMD(_ context.Context, res string, maxPeers int) error {
	f.j.add("drbd.create-md max-peers=%d", maxPeers)
	f.hasMD = f.createMDErr == nil
	return f.createMDErr
}

func (f *fakeDRBD) Up(context.Context, string) error {
	f.j.add("drbd.up")
	f.up = f.upErr == nil
	f.sync()
	return f.upErr
}

func (f *fakeDRBD) AdjustPending(context.Context, string) (bool, error) {
	f.j.add("drbd.adjust-pending")
	return f.pending, nil
}

func (f *fakeDRBD) Adjust(context.Context, string) error {
	f.j.add("drbd.adjust")
	f.pending = false
	return nil
}

func (f *fakeDRBD) Resize(context.Context, string) error {
	f.j.add("drbd.resize")
	f.sync()
	return nil
}

func (f *fakeDRBD) ForgetPeer(_ context.Context, res string, id int) error {
	f.j.add("drbd.forget-peer %d", id)
	return f.forgetErr
}

type rig struct {
	rt   *Runtime
	lvm  *fakeLVM
	drbd *fakeDRBD
	j    *journal
	dir  string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	j := &journal{}
	fl := &fakeLVM{j: j, lvs: map[string]lvm.LV{}}
	r := &rig{j: j, lvm: fl, drbd: &fakeDRBD{j: j, lvm: fl}, dir: t.TempDir()}
	r.rt = &Runtime{
		LVM: r.lvm, DRBD: r.drbd, VG: "vg0", Pool: "pool", ConfigDir: r.dir,
		SplitBrainCmd: "/run/current-system/sw/bin/expanse-drbd-event",
	}
	return r
}

func members(hosts ...string) []drbd.Member {
	ms := make([]drbd.Member, len(hosts))
	for i, h := range hosts {
		ms[i] = drbd.Member{Host: h, NodeID: i, Address: netip.MustParseAddr(fmt.Sprintf("192.168.1.%d", i+1))}
	}
	return ms
}

func desired() Desired {
	return Desired{
		Name: "vol-a1", SizeBytes: 64 << 20, Thin: true, Minor: 3, Port: 7793,
		Self: "n1", Members: members("n1", "n2", "n3"),
	}
}

func reconcile(t *testing.T, r *rig, d Desired) Result {
	t.Helper()
	res, err := r.rt.Reconcile(context.Background(), d)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return res
}

func TestReconcileTakesNothingToADefinedSecondary(t *testing.T) {
	r := newRig(t)
	res := reconcile(t, r, desired())
	want := []Action{CreateLV, WriteConfig, CreateMD, Up}
	if !reflect.DeepEqual(res.Actions, want) {
		t.Errorf("actions %v, want %v", res.Actions, want)
	}
	if !r.j.has(fmt.Sprintf("lvm.create-thin vg0/pool vol-a1 %d", backingSize(64<<20))) || !r.j.has("drbd.create-md max-peers=7") {
		t.Errorf("calls: %v", r.j.calls)
	}
	body, err := os.ReadFile(filepath.Join(r.dir, "vol-a1.res"))
	if err != nil {
		t.Fatal(err)
	}
	wantCfg, _ := drbd.Resource{
		Name: "vol-a1", Minor: 3, Port: 7793, Disk: "/dev/vg0/vol-a1",
		SplitBrainCmd: r.rt.SplitBrainCmd, Members: desired().Members,
	}.Render()
	if string(body) != wantCfg {
		t.Errorf("config differs from the generator's output:\n%s", body)
	}
}

func TestReconcileNeverPromotes(t *testing.T) {
	r := newRig(t)
	reconcile(t, r, desired())
	for _, c := range r.j.calls {
		if strings.Contains(c, "primary") || strings.Contains(c, "secondary") {
			t.Errorf("runtime touched the role: %s", c)
		}
	}
}

func TestSecondPassChangesNothing(t *testing.T) {
	r := newRig(t)
	reconcile(t, r, desired())
	r.j.calls = nil
	res := reconcile(t, r, desired())
	if res.Changed() || len(r.j.mutating()) != 0 {
		t.Errorf("second pass acted: actions %v, calls %v", res.Actions, r.j.calls)
	}
}

func TestThickVolumeWhenNotThin(t *testing.T) {
	r := newRig(t)
	d := desired()
	d.Thin = false
	reconcile(t, r, d)
	if !r.j.has(fmt.Sprintf("lvm.create-thick vg0 vol-a1 %d", backingSize(64<<20))) {
		t.Errorf("calls: %v", r.j.calls)
	}
}

func TestDriftIsCorrectedByAdjustAndOnlyThen(t *testing.T) {
	r := newRig(t)
	reconcile(t, r, desired())
	r.drbd.pending = true
	res := reconcile(t, r, desired())
	if !reflect.DeepEqual(res.Actions, []Action{Adjust}) {
		t.Errorf("actions %v, want [adjust]", res.Actions)
	}
	if again := reconcile(t, r, desired()); again.Changed() {
		t.Errorf("adjust did not settle: %v", again.Actions)
	}
}

func TestChangedMembershipRewritesTheConfig(t *testing.T) {
	r := newRig(t)
	reconcile(t, r, desired())
	r.drbd.pending = true // the kernel still has the old peer set
	d := desired()
	d.Members = members("n1", "n2")
	res := reconcile(t, r, d)
	if !reflect.DeepEqual(res.Actions, []Action{WriteConfig, Adjust}) {
		t.Errorf("actions %v, want [write-config adjust]", res.Actions)
	}
	if body, _ := os.ReadFile(filepath.Join(r.dir, "vol-a1.res")); strings.Contains(string(body), "n3") {
		t.Errorf("dropped peer still in the config:\n%s", body)
	}
}

func TestConfigWriteIsAtomicAndLeavesNoTempFiles(t *testing.T) {
	r := newRig(t)
	reconcile(t, r, desired())
	d := desired()
	d.Members = members("n1", "n2")
	reconcile(t, r, d)
	entries, _ := os.ReadDir(r.dir)
	if len(entries) != 1 || entries[0].Name() != "vol-a1.res" {
		t.Errorf("config dir holds %v", entries)
	}
}

func TestMetadataIsCreatedOnlyOnAPositiveBlankAnswer(t *testing.T) {
	cases := []struct {
		name       string
		hasMD      bool
		mdErr      error
		wantCreate bool
		wantErr    bool
	}{
		{"blank device", false, nil, true, false},
		{"metadata already there", true, nil, false, false},
		{"cannot tell", false, errors.New("drbdmeta: cannot open device"), false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.lvm.lvs["vol-a1"] = lvm.LV{Name: "vol-a1", Size: backingSize(64 << 20), Type: lvm.Thin} // pre-existing LV
			r.drbd.usableKiB = 64 << 10
			r.drbd.hasMD, r.drbd.mdErr = tc.hasMD, tc.mdErr
			_, err := r.rt.Reconcile(context.Background(), desired())
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, want error=%v", err, tc.wantErr)
			}
			if got := r.j.index("drbd.create-md max-peers=7") >= 0; got != tc.wantCreate {
				t.Errorf("create-md called = %v, want %v; calls %v", got, tc.wantCreate, r.j.calls)
			}
			if tc.wantErr && r.j.has("drbd.up") {
				t.Errorf("brought the resource up without knowing about its metadata")
			}
		})
	}
}

func TestFreshBackingDeviceAlwaysGetsFreshMetadata(t *testing.T) {
	r := newRig(t)
	r.drbd.hasMD = true // stale metadata left in recycled extents
	reconcile(t, r, desired())
	if !r.j.has("drbd.create-md max-peers=7") {
		t.Errorf("stale metadata on a new LV was trusted: %v", r.j.calls)
	}
}

func TestRunningResourceIsNeverProbedForMetadata(t *testing.T) {
	r := newRig(t)
	reconcile(t, r, desired())
	r.j.calls = nil
	reconcile(t, r, desired())
	if r.j.has("drbd.has-md") || r.j.index("drbd.create-md max-peers=7") >= 0 {
		t.Errorf("touched metadata of a running resource: %v", r.j.calls)
	}
}

func TestGrowingExtendsTheLVOnlyOnceTheResourceIsUpThenResizes(t *testing.T) {
	r := newRig(t)
	reconcile(t, r, desired())
	r.j.calls = nil
	d := desired()
	d.SizeBytes = 96 << 20
	res := reconcile(t, r, d)
	if !reflect.DeepEqual(res.Actions, []Action{ExtendLV, Resize}) {
		t.Fatalf("actions %v", res.Actions)
	}
	if r.j.index(fmt.Sprintf("lvm.extend vol-a1 %d", backingSize(96<<20))) > r.j.index("drbd.resize") {
		t.Errorf("resized before extending: %v", r.j.calls)
	}
}

func TestDownResourceIsBroughtUpBeforeItsDeviceGrows(t *testing.T) {
	r := newRig(t)
	reconcile(t, r, desired())
	r.drbd.up = false // e.g. after a reboot: LV and metadata exist, nothing is up
	r.j.calls = nil
	d := desired()
	d.SizeBytes = 96 << 20
	res := reconcile(t, r, d)
	if r.j.index("drbd.up") < 0 || r.j.index(fmt.Sprintf("lvm.extend vol-a1 %d", backingSize(96<<20))) < r.j.index("drbd.up") {
		t.Errorf("internal metadata would be stranded; order: %v (actions %v)", r.j.calls, res.Actions)
	}
	if r.j.has("drbd.create-md max-peers=7") {
		t.Error("recreated metadata on a device that already had it")
	}
}

func TestInterruptedGrowIsFinishedByTheNextPass(t *testing.T) {
	r := newRig(t)
	reconcile(t, r, desired())
	lv := r.lvm.lvs["vol-a1"]
	lv.Size = backingSize(96 << 20) // the LV grew, then the pass died before resize
	r.lvm.lvs["vol-a1"] = lv
	d := desired()
	d.SizeBytes = 96 << 20
	if res := reconcile(t, r, d); !reflect.DeepEqual(res.Actions, []Action{Resize}) {
		t.Errorf("actions %v, want [resize]", res.Actions)
	}
	if again := reconcile(t, r, d); again.Changed() {
		t.Errorf("resize did not settle: %v", again.Actions)
	}
}

func TestBackingDeviceIsLargerThanTheRequestByTheMetadataReserve(t *testing.T) {
	prev := uint64(0)
	for _, size := range []uint64{64 << 20, 1 << 30, 100 << 30, 4 << 40} {
		got := backingSize(size)
		if got <= size || got < prev || got-size > 2<<20+size/1000 {
			t.Errorf("backingSize(%d) = %d", size, got)
		}
		prev = got
	}
}

func TestShrinkRequestIsIgnored(t *testing.T) {
	r := newRig(t)
	reconcile(t, r, desired())
	d := desired()
	d.SizeBytes = 32 << 20
	if res := reconcile(t, r, d); res.Changed() {
		t.Errorf("shrink request acted: %v", res.Actions)
	}
}

func TestRetiredIDsAreForgottenAfterTheDeadPeerIsDropped(t *testing.T) {
	r := newRig(t)
	reconcile(t, r, desired())
	r.drbd.pending = true // the running config still has the dead peer
	r.j.calls = nil
	d := desired()
	d.Members = members("n1", "n2")
	d.Retired = []int{2, 9}
	res := reconcile(t, r, d)
	if !reflect.DeepEqual(res.Forgot, []int{2, 9}) {
		t.Errorf("forgot %v", res.Forgot)
	}
	if r.j.index("drbd.adjust") < 0 || r.j.index("drbd.adjust") > r.j.index("drbd.forget-peer 2") {
		t.Errorf("forgot before dropping the peer: %v", r.j.calls)
	}
}

func TestRetiredIDsAreForgottenOnFreshResourcesToo(t *testing.T) {
	r := newRig(t)
	d := desired()
	d.Members = members("n1", "n2")
	d.Retired = []int{2}
	res := reconcile(t, r, d)
	if r.j.index("drbd.up") > r.j.index("drbd.forget-peer 2") || !reflect.DeepEqual(res.Forgot, []int{2}) {
		t.Errorf("forget-peer needs the resource up: %v", r.j.calls)
	}
}

func TestForgetFailureIsAnErrorAndReportsNothingForgotten(t *testing.T) {
	r := newRig(t)
	r.drbd.forgetErr = errors.New("busy")
	d := desired()
	d.Retired = nil
	d.Members = members("n1", "n2")
	d.Retired = []int{2}
	res, err := r.rt.Reconcile(context.Background(), d)
	if err == nil || len(res.Forgot) != 0 {
		t.Errorf("got %v, forgot %v", err, res.Forgot)
	}
}

func TestErrorsStopThePassAndKeepTheActionsAlreadyTaken(t *testing.T) {
	r := newRig(t)
	r.drbd.createMDErr = errors.New("disk full")
	res, err := r.rt.Reconcile(context.Background(), desired())
	if err == nil {
		t.Fatal("want error")
	}
	if !reflect.DeepEqual(res.Actions, []Action{CreateLV, WriteConfig}) || r.j.has("drbd.up") {
		t.Errorf("actions %v, calls %v", res.Actions, r.j.calls)
	}
}

func TestLVFailureTouchesNothingElse(t *testing.T) {
	r := newRig(t)
	r.lvm.err = experrors.New(experrors.KindResourceExhausted, "fake", "no space")
	_, err := r.rt.Reconcile(context.Background(), desired())
	if experrors.KindOf(err) != experrors.KindResourceExhausted {
		t.Errorf("kind lost: %v", err)
	}
	if entries, _ := os.ReadDir(r.dir); len(entries) != 0 || r.j.has("drbd.up") {
		t.Errorf("state written despite the failure: %v %v", entries, r.j.calls)
	}
}

func TestInvalidDesiredStateNeverReachesTheSystem(t *testing.T) {
	cases := map[string]func(*Desired){
		"no size":              func(d *Desired) { d.SizeBytes = 0 },
		"self not a member":    func(d *Desired) { d.Self = "ghost" },
		"bad name":             func(d *Desired) { d.Name = "-r0" },
		"retired is live":      func(d *Desired) { d.Retired = []int{1} },
		"retired out of range": func(d *Desired) { d.Retired = []int{32} },
		"duplicate node-id":    func(d *Desired) { d.Members[1].NodeID = 0 },
		"bad port":             func(d *Desired) { d.Port = 80 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			d := desired()
			mutate(&d)
			_, err := r.rt.Reconcile(context.Background(), d)
			if experrors.KindOf(err) != experrors.KindInvalid || len(r.j.calls) != 0 {
				t.Errorf("want KindInvalid and no calls, got %v, %v", err, r.j.calls)
			}
		})
	}
}

func TestThinWithoutAPoolIsInvalid(t *testing.T) {
	r := newRig(t)
	r.rt.Pool = ""
	_, err := r.rt.Reconcile(context.Background(), desired())
	if experrors.KindOf(err) != experrors.KindInvalid {
		t.Errorf("got %v", err)
	}
}

func TestConfigThatCannotBeStagedLeavesTheOldOneIntact(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	r := newRig(t)
	reconcile(t, r, desired())
	path := filepath.Join(r.dir, "vol-a1.res")
	before, _ := os.ReadFile(path)
	os.Chmod(r.dir, 0o555)
	t.Cleanup(func() { os.Chmod(r.dir, 0o755) })
	d := desired()
	d.Members = members("n1", "n2")
	if _, err := r.rt.Reconcile(context.Background(), d); err == nil {
		t.Fatal("want error")
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Errorf("config was modified in place:\n%s", after)
	}
}
