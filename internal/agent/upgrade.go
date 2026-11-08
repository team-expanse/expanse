package agent

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"slices"
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
	// rebootNeeded reports whether the running system waits for a reboot; nil means never.
	rebootNeeded func() bool
	// bootID is the kernel's boot ID; reboot reboots this node; rebooter is the leader's side.
	bootID    func() string
	reboot    func(ctx context.Context) error
	rebooter  rebooter
	rebooting bool // this node asked systemd to reboot it
	// releases lists the published releases, newest first; nil turns the check off.
	releases func(ctx context.Context) ([]string, error)
	log      *slog.Logger

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
	if u.isLeader() {
		u.checkLatest(ctx, st, now)
	}
	jobs, err := rollout.ListReboots(ctx, st)
	if err != nil {
		return
	}
	u.rebootIfAsked(ctx, jobs[u.node], rep.BootID)
	plan, err := rollout.LoadPlan(ctx, st)
	if err != nil {
		return
	}
	if plan != nil && rollout.ShouldStart(*plan, u.node, rep) {
		u.logger().Info("starting this node's upgrade", "target", plan.Target)
		if err := u.start(ctx, plan.Target); err != nil {
			u.logger().Error("upgrade did not start", "target", plan.Target, "err", err)
		}
	}
	if u.isLeader() {
		u.lead(ctx, st, plan, jobs, now)
	}
}

// rebootIfAsked reboots this node once its reboot says so.
func (u *nodeUpgrades) rebootIfAsked(ctx context.Context, j rollout.Reboot, bootID string) {
	if u.rebooting || u.reboot == nil || !rollout.ShouldReboot(j, u.node, bootID) {
		return
	}
	u.logger().Info("rebooting this node", "in_place", j.InPlace, "held", len(j.Held))
	if err := u.reboot(ctx); err != nil {
		u.logger().Error("reboot did not start", "err", err)
		return
	}
	u.rebooting = true
}

func (u *nodeUpgrades) lead(ctx context.Context, st store.Store, plan *rollout.Plan, jobs map[string]rollout.Reboot, now time.Time) {
	reports, err := rollout.ListReports(ctx, st)
	if err != nil {
		return
	}
	h := u.health(ctx)
	if plan != nil && rollout.NeedsRebootJob(*plan, reports, jobs) {
		j := rollout.NewReboot(plan.Current, jobs[plan.Current], now)
		if err := rollout.SaveReboot(ctx, st, j); err != nil {
			u.logger().Warn("reboot not started", "node", j.Node, "err", err)
		} else {
			jobs[j.Node] = j
		}
	}
	u.leadReboots(ctx, st, jobs, reports, h, now)
	if plan == nil {
		return
	}
	next, changed := rollout.Step(*plan, reports, jobs, h, now)
	if !changed {
		return
	}
	if err := rollout.SavePlan(ctx, st, next); err != nil {
		u.logger().Warn("rollout not saved", "err", err)
		return
	}
	u.logger().Info("rollout", "target", next.Target, "state", next.State, "current", next.Current, "reason", next.Reason)
}

// leadReboots advances every open node reboot, beginning a pending one only while none other
// is underway and the cluster is healthy.
func (u *nodeUpgrades) leadReboots(ctx context.Context, st store.Store, jobs map[string]rollout.Reboot, reports map[string]rollout.Report, h rollout.Health, now time.Time) {
	if u.rebooter == nil {
		return
	}
	for _, n := range slices.Sorted(maps.Keys(jobs)) {
		j := jobs[n]
		if !j.Open() && !(j.Cancel && j.State == rollout.RebootFailed) {
			continue
		}
		next, err := u.stepReboot(ctx, j, jobs, reports[n], h, now)
		if err != nil {
			u.logger().Warn("reboot not advanced", "node", n, "err", err)
			continue
		}
		if reflect.DeepEqual(next, j) {
			continue
		}
		if err := rollout.SaveReboot(ctx, st, next); err != nil {
			u.logger().Warn("reboot not saved", "node", n, "err", err)
			continue
		}
		jobs[n] = next
		u.logger().Info("reboot", "node", n, "state", next.State, "reason", next.Reason)
	}
}

func (u *nodeUpgrades) stepReboot(ctx context.Context, j rollout.Reboot, jobs map[string]rollout.Reboot, rep rollout.Report, h rollout.Health, now time.Time) (rollout.Reboot, error) {
	if j.Cancel && j.State != rollout.RebootRebooting { // a node already going down cannot be called back
		// A failed reboot may have left the node drained; an in-place one never drains it.
		if (j.State == rollout.RebootDraining || j.State == rollout.RebootFailed) && !j.InPlace {
			if err := u.rebooter.end(ctx, j); err != nil {
				return j, err
			}
		}
		j.State, j.Reason, j.Updated = rollout.RebootCancelled, "", now
		return j, nil
	}
	if j.State == rollout.RebootPending {
		if why := rollout.CanBegin(j, jobs, h); why != "" {
			j.Reason = why
			return j, nil
		}
		if rep.BootID == "" {
			j.State, j.Reason = rollout.RebootFailed, j.Node+": reports no boot ID; it runs an Expanse too old to reboot this way"
			return j, nil
		}
		j.BootID, j.Reason, j.Since, j.Updated = rep.BootID, "", now, now
		return u.rebooter.begin(ctx, j)
	}
	var d rollout.Drain
	if j.State == rollout.RebootDraining {
		var err error
		if d, err = u.rebooter.progress(ctx, j); err != nil {
			return j, err
		}
	}
	next, _ := rollout.StepReboot(j, rep, d, now)
	if next.State == rollout.RebootDone && !j.InPlace {
		if err := u.rebooter.end(ctx, j); err != nil {
			return j, err // still drained: the next pass tries again
		}
	}
	return next, nil
}

// checkLatest records the published releases when a check is due.
func (u *nodeUpgrades) checkLatest(ctx context.Context, st store.Store, now time.Time) {
	l, err := rollout.LoadLatest(ctx, st)
	if err != nil || u.releases == nil || !l.Due(now) {
		return
	}
	vs, err := u.releases(ctx)
	l.Checked, l.Requested, l.Error = now, false, ""
	if err != nil {
		l.Error = err.Error()
	} else {
		l.Version, l.Releases = vs[0], vs
	}
	if err := rollout.SaveLatest(ctx, st, l); err != nil {
		u.logger().Warn("newest release not recorded", "err", err)
	}
}

func (u *nodeUpgrades) report(now time.Time) rollout.Report {
	rep := rollout.Report{Version: u.version, Updated: now}
	if u.bootID != nil {
		rep.BootID = u.bootID()
	}
	if u.rebootNeeded != nil {
		rep.RebootNeeded = u.rebootNeeded()
	}
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
	return a.Version == b.Version && a.RebootNeeded == b.RebootNeeded && a.BootID == b.BootID && reflect.DeepEqual(a.Upgrade, b.Upgrade)
}

// startUpgrade runs `expanse upgrade start`, which detaches the upgrade into its own unit.
func (a *Agent) startUpgrade(ctx context.Context, version string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"upgrade", "start", version}
	if a.cfg.ReleaseURL != "" {
		args = append(args, "--base-url", a.cfg.ReleaseURL)
	}
	if a.cfg.ReleaseKey != "" {
		args = append(args, "--key", a.cfg.ReleaseKey)
	}
	out, err := exec.CommandContext(ctx, self, args...).CombinedOutput()
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
		start:    a.startUpgrade,
		isLeader: func() bool { return a.ctl.store.IsLeader() },
		health:   a.rolloutHealth,
		rebootNeeded: func() bool {
			return upgrade.RebootNeeded("/run/booted-system", "/run/current-system")
		},
		bootID:   bootID,
		reboot:   rebootNode,
		rebooter: clusterRebooter{st: a.ctl.store},
		releases: func(ctx context.Context) ([]string, error) {
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second) // the loop must not hang on GitHub
			defer cancel()
			return upgrade.FetchReleases(ctx, http.DefaultClient, upgrade.ReleasesURL)
		},
		log: a.logger,
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
