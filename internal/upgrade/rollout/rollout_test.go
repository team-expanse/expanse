package rollout

import (
	"strings"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/upgrade"
)

var now = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

func running(order ...string) Plan {
	return Plan{Target: "1.12.0", State: Running, Order: order, Started: now}
}

func at(version string) Report { return Report{Version: version, Updated: now} }

func upgrading(state upgrade.State) Report {
	return Report{
		Version: "1.11.3", Updated: now,
		Upgrade: &upgrade.Status{Version: "1.12.0", State: state, Started: now, Updated: now},
	}
}

var healthy = Health{OK: true}

func TestStepPicksTheFirstNodeNotYetOnTheTarget(t *testing.T) {
	p, changed := Step(running("n1", "n2", "n3"), map[string]Report{"n1": at("1.12.0"), "n2": at("1.11.3"), "n3": at("1.11.3")}, healthy, now)
	if !changed || p.Current != "n2" {
		t.Fatalf("plan = %+v", p)
	}
}

func TestStepWaitsForAHealthyClusterBeforeStartingANode(t *testing.T) {
	p, _ := Step(running("n1", "n2"), map[string]Report{"n1": at("1.11.3"), "n2": at("1.11.3")}, Health{Why: "volume db is resyncing"}, now)
	if p.Current != "" || p.State != Running || !strings.Contains(p.Reason, "resyncing") {
		t.Fatalf("plan = %+v", p)
	}
}

func TestStepMovesOnOnceTheNodeRunsTheTargetAndTheClusterIsHealthy(t *testing.T) {
	plan := running("n1", "n2")
	plan.Current = "n1"
	reports := map[string]Report{"n1": at("1.12.0"), "n2": at("1.11.3")}
	reports["n1"] = Report{Version: "1.12.0", Updated: now, Upgrade: &upgrade.Status{Version: "1.12.0", State: upgrade.StateDone}}
	p, _ := Step(plan, reports, Health{Why: "n1 rejoining"}, now)
	if p.Current != "n1" {
		t.Fatalf("moved on while the cluster was unhealthy: %+v", p)
	}
	p, _ = Step(plan, reports, healthy, now)
	if p.Current != "n2" {
		t.Fatalf("plan = %+v", p)
	}
}

func TestStepIsDoneWhenEveryNodeRunsTheTarget(t *testing.T) {
	p, _ := Step(running("n1", "n2"), map[string]Report{"n1": at("1.12.0"), "n2": at("1.12.0")}, healthy, now)
	if p.State != Done || p.Current != "" {
		t.Fatalf("plan = %+v", p)
	}
}

func TestStepHaltsOnAFailedNode(t *testing.T) {
	plan := running("n1", "n2")
	plan.Current = "n1"
	r := upgrading(upgrade.StateFailed)
	r.Upgrade.Error = "build: builder failed"
	p, _ := Step(plan, map[string]Report{"n1": r, "n2": at("1.11.3")}, healthy, now)
	if p.State != Halted || !strings.Contains(p.Reason, "n1") || !strings.Contains(p.Reason, "builder failed") {
		t.Fatalf("plan = %+v", p)
	}
}

func TestStepHaltsWhenANodeStopsReporting(t *testing.T) {
	plan := running("n1", "n2")
	plan.Current = "n1"
	r := upgrading(upgrade.StateSwitching)
	r.Updated = now.Add(-NodeTimeout - time.Minute)
	r.Upgrade.Updated = r.Updated
	p, _ := Step(plan, map[string]Report{"n1": r, "n2": at("1.11.3")}, healthy, now)
	if p.State != Halted || !strings.Contains(p.Reason, "n1") {
		t.Fatalf("plan = %+v", p)
	}
}

func TestStepLeavesAPausedPlanAlone(t *testing.T) {
	plan := running("n1")
	plan.State = Paused
	if _, changed := Step(plan, map[string]Report{"n1": at("1.11.3")}, healthy, now); changed {
		t.Fatal("a paused plan advanced")
	}
}

func TestShouldStartOnlyTheCurrentNodeAndOnlyOnce(t *testing.T) {
	plan := running("n1", "n2")
	plan.Current = "n1"
	if ShouldStart(plan, "n2", at("1.11.3")) {
		t.Error("a node that is not current started")
	}
	if !ShouldStart(plan, "n1", at("1.11.3")) {
		t.Error("the current node did not start")
	}
	if ShouldStart(plan, "n1", upgrading(upgrade.StateBuilding)) {
		t.Error("started twice")
	}
	if ShouldStart(plan, "n1", upgrading(upgrade.StateFailed)) {
		t.Error("retried a failure without being asked")
	}
	if ShouldStart(plan, "n1", at("1.12.0")) {
		t.Error("upgraded a node already on the target")
	}
}

func TestOrderPutsTheLeaderLast(t *testing.T) {
	got := Order([]string{"n3", "n1", "n2"}, "n1")
	if strings.Join(got, ",") != "n2,n3,n1" {
		t.Errorf("order = %v", got)
	}
}

func TestShouldStartAgainAfterTheRolloutIsResumed(t *testing.T) {
	plan := running("n1")
	plan.Current = "n1"
	failed := upgrading(upgrade.StateFailed)
	plan.Started = now.Add(time.Minute) // resumed after the failure
	if !ShouldStart(plan, "n1", failed) {
		t.Error("a resumed rollout did not retry the failed node")
	}
}
