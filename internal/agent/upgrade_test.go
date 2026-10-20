package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/store/boltstore"
	"github.com/expanse/expanse/internal/upgrade"
	"github.com/expanse/expanse/internal/upgrade/rollout"
)

type upgradeRig struct {
	u       *nodeUpgrades
	st      *boltstore.Store
	started []string
	leader  bool
	health  rollout.Health
}

func newUpgradeRig(t *testing.T, node, version string) *upgradeRig {
	t.Helper()
	st, err := boltstore.New(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	r := &upgradeRig{st: st, health: rollout.Health{OK: true}}
	r.u = &nodeUpgrades{
		node: node, version: version, statusPath: filepath.Join(t.TempDir(), "upgrade.json"),
		start:    func(_ context.Context, v string) error { r.started = append(r.started, v); return nil },
		isLeader: func() bool { return r.leader },
		health:   func(context.Context) rollout.Health { return r.health },
	}
	return r
}

func TestUpgradePassReportsThisNode(t *testing.T) {
	r := newUpgradeRig(t, "n1", "1.11.3")
	ctx := context.Background()
	r.u.pass(ctx, r.st, time.Now())
	got, _ := rollout.ListReports(ctx, r.st)
	if got["n1"].Version != "1.11.3" || got["n1"].Upgrade != nil {
		t.Fatalf("reports = %+v", got)
	}
	st := upgrade.Status{Version: "1.12.0", State: upgrade.StateBuilding}
	b, _ := json.Marshal(st)
	os.WriteFile(r.u.statusPath, b, 0o644)
	r.u.pass(ctx, r.st, time.Now())
	got, _ = rollout.ListReports(ctx, r.st)
	if got["n1"].Upgrade == nil || got["n1"].Upgrade.State != upgrade.StateBuilding {
		t.Fatalf("reports = %+v", got)
	}
}

func TestUpgradePassStartsThisNodeWhenTheLeaderNamesIt(t *testing.T) {
	r := newUpgradeRig(t, "n1", "1.11.3")
	ctx := context.Background()
	r.leader = true
	plan := rollout.Plan{Target: "1.12.0", State: rollout.Running, Order: []string{"n1"}, Started: time.Now()}
	if err := rollout.SavePlan(ctx, r.st, plan); err != nil {
		t.Fatal(err)
	}
	r.u.pass(ctx, r.st, time.Now()) // reports, then the leader picks n1
	r.u.pass(ctx, r.st, time.Now()) // n1 sees itself named
	if len(r.started) != 1 || r.started[0] != "1.12.0" {
		t.Fatalf("started = %v", r.started)
	}
}

func TestUpgradePassOnAFollowerLeavesThePlanAlone(t *testing.T) {
	r := newUpgradeRig(t, "n2", "1.11.3")
	ctx := context.Background()
	plan := rollout.Plan{Target: "1.12.0", State: rollout.Running, Order: []string{"n2"}, Started: time.Now()}
	rollout.SavePlan(ctx, r.st, plan)
	r.u.pass(ctx, r.st, time.Now())
	got, _ := rollout.LoadPlan(ctx, r.st)
	if got.Current != "" || len(r.started) != 0 {
		t.Fatalf("a follower moved the plan: %+v, started %v", got, r.started)
	}
}
