package agent

import (
	"context"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/upgrade/rollout"
)

// fakeRebooter records the leader's drains and reports a drain's progress as set.
type fakeRebooter struct {
	inPlace      bool
	moving       []string
	began, ended []string
}

func (f *fakeRebooter) begin(_ context.Context, r rollout.Reboot) (rollout.Reboot, error) {
	f.began = append(f.began, r.Node)
	r.InPlace = f.inPlace
	r.State = rollout.RebootDraining
	if f.inPlace {
		r.State = rollout.RebootRebooting
	}
	return r, nil
}

func (f *fakeRebooter) progress(context.Context, rollout.Reboot) (rollout.Drain, error) {
	return rollout.Drain{Moving: f.moving}, nil
}

func (f *fakeRebooter) end(_ context.Context, r rollout.Reboot) error {
	f.ended = append(f.ended, r.Node)
	return nil
}

// rebootRig is an upgrade rig whose node reboots on request, into a new boot ID.
func rebootRig(t *testing.T) (*upgradeRig, *fakeRebooter, *int, *string) {
	r := newUpgradeRig(t, "n1", "1.12.0")
	f := &fakeRebooter{}
	reboots, boot := 0, "boot-1"
	needed := true
	r.u.rebooter = f
	r.u.bootID = func() string { return boot }
	r.u.rebootNeeded = func() bool { return needed && boot == "boot-1" }
	r.u.reboot = func(context.Context) error { reboots++; return nil }
	return r, f, &reboots, &boot
}

func loadReboot(t *testing.T, r *upgradeRig) rollout.Reboot {
	t.Helper()
	all, err := rollout.ListReboots(context.Background(), r.st)
	if err != nil {
		t.Fatal(err)
	}
	return all["n1"]
}

func TestTheLeaderDrainsRebootsAndRestoresANode(t *testing.T) {
	r, f, reboots, boot := rebootRig(t)
	ctx := context.Background()
	r.leader = true
	r.u.pass(ctx, r.st, time.Now()) // report the boot ID first
	if err := rollout.SaveReboot(ctx, r.st, rollout.Reboot{Node: "n1", State: rollout.RebootPending, Started: time.Now(), Since: time.Now()}); err != nil {
		t.Fatal(err)
	}
	f.moving = []string{"default/web replica 0"}
	r.u.pass(ctx, r.st, time.Now())
	if j := loadReboot(t, r); j.State != rollout.RebootDraining || j.BootID != "boot-1" || len(f.began) != 1 {
		t.Fatalf("job = %+v, began %v; want draining from boot-1", j, f.began)
	}
	f.moving = nil
	r.u.pass(ctx, r.st, time.Now()) // drained: the leader marks it rebooting
	r.u.pass(ctx, r.st, time.Now()) // the node sees it and reboots
	r.u.pass(ctx, r.st, time.Now()) // a second pass must not reboot it again
	if *reboots != 1 {
		t.Fatalf("reboots = %d, want 1", *reboots)
	}
	*boot = "boot-2"
	r.u.pass(ctx, r.st, time.Now()) // back: report the new boot
	r.u.pass(ctx, r.st, time.Now())
	if j := loadReboot(t, r); j.State != rollout.RebootDone || len(f.ended) != 1 {
		t.Fatalf("job = %+v, ended %v; want done and back in service", j, f.ended)
	}
}

func TestAPendingRebootWaitsForAHealthyCluster(t *testing.T) {
	r, f, _, _ := rebootRig(t)
	ctx := context.Background()
	r.leader, r.health = true, rollout.Health{Why: "volume db is Degraded"}
	if err := rollout.SaveReboot(ctx, r.st, rollout.Reboot{Node: "n1", State: rollout.RebootPending, Started: time.Now()}); err != nil {
		t.Fatal(err)
	}
	r.u.pass(ctx, r.st, time.Now())
	if j := loadReboot(t, r); j.State != rollout.RebootPending || j.Reason == "" || len(f.began) != 0 {
		t.Fatalf("job = %+v, want pending with the reason", j)
	}
}

func TestAnInPlaceRebootSkipsTheDrain(t *testing.T) {
	r, f, reboots, _ := rebootRig(t)
	ctx := context.Background()
	r.leader, f.inPlace = true, true
	r.u.pass(ctx, r.st, time.Now())
	if err := rollout.SaveReboot(ctx, r.st, rollout.Reboot{Node: "n1", State: rollout.RebootPending, Started: time.Now()}); err != nil {
		t.Fatal(err)
	}
	r.u.pass(ctx, r.st, time.Now())
	r.u.pass(ctx, r.st, time.Now())
	if *reboots != 1 {
		t.Fatalf("reboots = %d, want 1 without a drain", *reboots)
	}
}

func TestAnAutomatedRolloutStartsTheCurrentNodesReboot(t *testing.T) {
	r, _, _, _ := rebootRig(t)
	ctx := context.Background()
	r.leader = true
	now := time.Now()
	plan := rollout.Plan{Target: "1.12.0", State: rollout.Running, Order: []string{"n1"}, Current: "n1", CurrentSince: now, Reboot: true, Started: now}
	if err := rollout.SavePlan(ctx, r.st, plan); err != nil {
		t.Fatal(err)
	}
	r.u.pass(ctx, r.st, now)
	if j := loadReboot(t, r); j.Node != "n1" || !j.Open() {
		t.Fatalf("job = %+v, want n1's reboot started", j)
	}
}
