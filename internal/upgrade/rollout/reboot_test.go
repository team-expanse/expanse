package rollout

import (
	"strings"
	"testing"
	"time"
)

func job(state RebootState, since time.Time) Reboot {
	return Reboot{Node: "n1", State: state, BootID: "boot-1", Since: since, Started: since}
}

func booted(id string, needed bool) Report {
	return Report{Version: "1.12.0", BootID: id, RebootNeeded: needed, Updated: now}
}

func TestAPendingRebootWaitsForAHealthyClusterAndNoOtherReboot(t *testing.T) {
	j := job(RebootPending, now)
	if why := CanBegin(j, nil, Health{Why: "volume db is resyncing"}); !strings.Contains(why, "resyncing") {
		t.Errorf("CanBegin on an unhealthy cluster = %q", why)
	}
	other := Reboot{Node: "n2", State: RebootDraining}
	if why := CanBegin(j, map[string]Reboot{"n2": other}, healthy); !strings.Contains(why, "n2") {
		t.Errorf("CanBegin while n2 reboots = %q", why)
	}
	done := Reboot{Node: "n2", State: RebootDone}
	if why := CanBegin(j, map[string]Reboot{"n2": done, "n1": j}, healthy); why != "" {
		t.Errorf("CanBegin = %q, want free to begin", why)
	}
}

func TestADrainEndsOnceNothingIsLeftToMove(t *testing.T) {
	j := job(RebootDraining, now)
	j, changed := StepReboot(j, booted("boot-1", true), Drain{Moving: []string{"default/web replica 0"}}, now.Add(time.Minute))
	if changed || j.State != RebootDraining {
		t.Fatalf("job = %+v, want still draining", j)
	}
	held := map[string]string{"default/db replica 0": "no other node holds a healthy copy of its volumes"}
	j, changed = StepReboot(j, booted("boot-1", true), Drain{Held: held}, now.Add(2*time.Minute))
	if !changed || j.State != RebootRebooting || j.Held["default/db replica 0"] == "" || !j.Since.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("job = %+v, want rebooting with the held replica listed", j)
	}
}

func TestADrainThatStallsFailsNamingWhatDidNotMove(t *testing.T) {
	j := job(RebootDraining, now)
	j, _ = StepReboot(j, booted("boot-1", true), Drain{Moving: []string{"default/web replica 0"}}, now.Add(DrainTimeout+time.Second))
	if j.State != RebootFailed || !strings.Contains(j.Reason, "default/web replica 0") {
		t.Fatalf("job = %+v, want failed naming the replica", j)
	}
}

func TestARebootIsDoneOnceTheNodeIsBackOnItsNewBoot(t *testing.T) {
	j := job(RebootRebooting, now)
	if j, changed := StepReboot(j, booted("boot-1", true), Drain{}, now.Add(time.Minute)); changed {
		t.Fatalf("job = %+v, want waiting for the node to reboot", j)
	}
	j, changed := StepReboot(j, booted("boot-2", false), Drain{}, now.Add(3*time.Minute))
	if !changed || j.State != RebootDone {
		t.Fatalf("job = %+v, want done", j)
	}
}

func TestANodeBackStillWaitingForARebootFails(t *testing.T) {
	j, _ := StepReboot(job(RebootRebooting, now), booted("boot-2", true), Drain{}, now.Add(3*time.Minute))
	if j.State != RebootFailed || !strings.Contains(j.Reason, "still") {
		t.Fatalf("job = %+v, want failed: the reboot did not load the new system", j)
	}
}

func TestANodeThatDoesNotComeBackFails(t *testing.T) {
	j, _ := StepReboot(job(RebootRebooting, now), booted("boot-1", true), Drain{}, now.Add(BackTimeout+time.Second))
	if j.State != RebootFailed || !strings.Contains(j.Reason, "not back") {
		t.Fatalf("job = %+v, want failed", j)
	}
}

func TestAutomatedRolloutRebootsANodeBeforeMovingOn(t *testing.T) {
	p := running("n1", "n2")
	p.Reboot, p.Current = true, "n1"
	reports := map[string]Report{"n1": booted("boot-1", true), "n2": at("1.11.3")}
	p, _ = Step(p, reports, nil, healthy, now)
	if p.Current != "n1" || !strings.Contains(p.Reason, "reboot") {
		t.Fatalf("plan = %+v, want waiting on n1's reboot", p)
	}
	j := job(RebootFailed, now)
	j.Reason = "n1: not back from its reboot after 15m0s"
	p, _ = Step(p, reports, map[string]Reboot{"n1": j}, healthy, now)
	if p.State != Halted || !strings.Contains(p.Reason, "not back") {
		t.Fatalf("plan = %+v, want halted with the reboot's reason", p)
	}
	p.State, p.Started = Running, now.Add(time.Minute) // Retry: the old failure no longer counts
	p, _ = Step(p, reports, map[string]Reboot{"n1": j}, healthy, now.Add(time.Minute))
	if p.State != Running || p.Current != "n1" {
		t.Fatalf("plan = %+v after a retry, want waiting on n1 again", p)
	}
	reports["n1"] = booted("boot-2", false)
	p, _ = Step(p, reports, map[string]Reboot{"n1": job(RebootDone, now)}, healthy, now.Add(time.Minute))
	if p.Current != "n2" {
		t.Fatalf("plan = %+v, want on to n2", p)
	}
}

func TestAutomatedRolloutRebootsANodeAlreadyOnTheTarget(t *testing.T) {
	p := running("n1", "n2")
	p.Reboot = true
	p, _ = Step(p, map[string]Report{"n1": booted("boot-1", true), "n2": at("1.12.0")}, nil, healthy, now)
	if p.Current != "n1" {
		t.Fatalf("plan = %+v, want n1 picked for its reboot", p)
	}
}

func TestARolloutWithoutRebootsLeavesThemToTheOperator(t *testing.T) {
	p := running("n1", "n2")
	p.Current = "n1"
	p, _ = Step(p, map[string]Report{"n1": booted("boot-1", true), "n2": at("1.11.3")}, nil, healthy, now)
	if p.Current != "n2" {
		t.Fatalf("plan = %+v, want on to n2 without a reboot", p)
	}
}

func TestRebootJobsToStart(t *testing.T) {
	p := running("n1", "n2")
	p.Reboot, p.Current = true, "n1"
	reports := map[string]Report{"n1": booted("boot-1", true)}
	if !NeedsRebootJob(p, reports, nil) {
		t.Error("no job for n1 yet: want one started")
	}
	if NeedsRebootJob(p, reports, map[string]Reboot{"n1": job(RebootDraining, now)}) {
		t.Error("n1's reboot is underway: want no second job")
	}
	stale := job(RebootFailed, now.Add(-time.Hour))
	if !NeedsRebootJob(p, reports, map[string]Reboot{"n1": stale}) {
		t.Error("n1's job failed before this rollout's (re)start: want a new one")
	}
	p.Reboot = false
	if NeedsRebootJob(p, reports, nil) {
		t.Error("a rollout without reboots starts none")
	}
}
