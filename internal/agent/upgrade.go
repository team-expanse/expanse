package agent

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"reflect"
	"time"

	"github.com/expanse/expanse/internal/cluster/control"
	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/upgrade"
	"github.com/expanse/expanse/internal/upgrade/rollout"
	"github.com/expanse/expanse/internal/version"
)

const (
	upgradePassInterval = 10 * time.Second
	// upgradeHeartbeat rewrites an unchanged report, so the leader can tell a silent node.
	upgradeHeartbeat = 5 * time.Minute
)

// nodeUpgrades is this node's part in a rollout: report itself, start its own upgrade when the
// plan names it, and on the leader advance the plan.
type nodeUpgrades struct {
	node, version, statusPath string
	start                     func(ctx context.Context, version string) error
	isLeader                  func() bool
	health                    func(ctx context.Context) rollout.Health
	log                       *slog.Logger

	last      *rollout.Report
	lastSaved time.Time
}

func (u *nodeUpgrades) pass(ctx context.Context, st store.Store, now time.Time) {
	rep := u.report(now)
	if u.last == nil || !sameReport(*u.last, rep) || now.Sub(u.lastSaved) > upgradeHeartbeat {
		if err := rollout.SaveReport(ctx, st, u.node, rep); err != nil {
			u.logger().Warn("upgrade report not written", "err", err)
		} else {
			u.last, u.lastSaved = &rep, now
		}
	}
	plan, err := rollout.LoadPlan(ctx, st)
	if err != nil || plan == nil {
		return
	}
	if rollout.ShouldStart(*plan, u.node, rep) {
		u.logger().Info("starting this node's upgrade", "target", plan.Target)
		if err := u.start(ctx, plan.Target); err != nil {
			u.logger().Error("upgrade did not start", "target", plan.Target, "err", err)
		}
	}
	if u.isLeader() {
		u.lead(ctx, st, *plan, now)
	}
}

func (u *nodeUpgrades) lead(ctx context.Context, st store.Store, plan rollout.Plan, now time.Time) {
	reports, err := rollout.ListReports(ctx, st)
	if err != nil {
		return
	}
	next, changed := rollout.Step(plan, reports, u.health(ctx), now)
	if !changed {
		return
	}
	if err := rollout.SavePlan(ctx, st, next); err != nil {
		u.logger().Warn("rollout not saved", "err", err)
		return
	}
	u.logger().Info("rollout", "target", next.Target, "state", next.State, "current", next.Current, "reason", next.Reason)
}

func (u *nodeUpgrades) report(now time.Time) rollout.Report {
	rep := rollout.Report{Version: u.version, Updated: now}
	st, err := upgrade.ReadStatus(u.statusPath)
	switch {
	case err == nil:
		rep.Upgrade = st
	case !errors.Is(err, fs.ErrNotExist):
		u.logger().Warn("upgrade status unreadable", "err", err)
	}
	return rep
}

func (u *nodeUpgrades) logger() *slog.Logger {
	if u.log != nil {
		return u.log
	}
	return slog.Default()
}

func sameReport(a, b rollout.Report) bool {
	return a.Version == b.Version && reflect.DeepEqual(a.Upgrade, b.Upgrade)
}

// startUpgrade runs `expanse upgrade start`, which detaches the upgrade into its own unit.
func startUpgrade(ctx context.Context, version string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, self, "upgrade", "start", version).CombinedOutput()
	if err != nil {
		return errors.Join(err, errors.New(string(out)))
	}
	return nil
}

// rolloutHealth reads the cluster's status and every volume's state for the rollout gate.
func (a *Agent) rolloutHealth(ctx context.Context) rollout.Health {
	rep, err := control.Status(ctx, a.ctl.store)
	if err != nil {
		return rollout.Health{Why: "cluster status: " + err.Error()}
	}
	ids, err := storage.ListVolumeIDs(ctx, a.store)
	if err != nil {
		return rollout.Health{Why: "volumes: " + err.Error()}
	}
	vols := make(map[string]storage.VolumeState, len(ids))
	for _, id := range ids {
		st, _, err := storage.LoadStatus(ctx, a.store, id)
		if err != nil {
			return rollout.Health{Why: "volume " + id + ": " + err.Error()}
		}
		name := id
		if spec, err := storage.LoadSpec(ctx, a.store, id); err == nil && spec.Name != "" {
			name = spec.Name
		}
		vols[name] = st.State
	}
	return rollout.ClusterHealth(rep, vols)
}

// upgradeLoop runs this node's part in rollouts until ctx ends.
func (a *Agent) upgradeLoop(ctx context.Context) {
	u := &nodeUpgrades{
		node: a.cfg.NodeID, version: version.Get().Version, statusPath: upgrade.DefaultStatusPath,
		start:    startUpgrade,
		isLeader: func() bool { return a.ctl.store.IsLeader() },
		health:   a.rolloutHealth,
		log:      a.logger,
	}
	tick := time.NewTicker(upgradePassInterval)
	defer tick.Stop()
	for {
		u.pass(ctx, a.store, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
