package web

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/upgrade"
	"github.com/expanse/expanse/internal/upgrade/rollout"
)

func (s *Server) registerUpgradeRoutes() {
	s.routes.Handle("GET /upgrade", s.requireAuth(http.HandlerFunc(s.handleUpgrade)))
	s.routes.Handle("GET /upgrade/events", s.requireAuth(http.HandlerFunc(s.handleUpgradeEvents)))
	s.routes.Handle("POST /upgrade/start", s.requireAuth(http.HandlerFunc(s.handleUpgradeStart)))
	s.routes.Handle("POST /upgrade/pause", s.requireAuth(s.planChange(rollout.Running, s.pausePlan, "Rollout paused; the node upgrading now finishes first.")))
	s.routes.Handle("POST /upgrade/resume", s.requireAuth(s.planChange("", s.resumePlan, "Rollout resumed.")))
	s.routes.Handle("POST /upgrade/cancel", s.requireAuth(s.planChange("", s.cancelPlan, "Rollout cancelled; nodes already upgraded stay upgraded.")))
	s.routes.Handle("POST /upgrade/check", s.requireAuth(http.HandlerFunc(s.handleUpgradeCheck)))
	s.routes.Handle("POST /upgrade/auto-check", s.requireAuth(http.HandlerFunc(s.handleUpgradeAutoCheck)))
}

// upgradeNode is one row of the versions table.
type upgradeNode struct {
	ID      string
	Report  rollout.Report
	Current bool
}

type upgradeView struct {
	Latest  rollout.Latest
	Plan    *rollout.Plan
	Nodes   []upgradeNode
	Offer   string   // the version the start form proposes
	Errors  []string // every read that failed, so none hides another
	Running bool     // a rollout is running or paused, so no new one can start
}

type upgradeData struct {
	Page page
	View upgradeView
}

func (s *Server) loadUpgrade(ctx context.Context) upgradeView {
	ctx = store.WithStale(ctx)
	var v upgradeView
	var err error
	if v.Latest, err = rollout.LoadLatest(ctx, s.store); err != nil {
		v.Errors = append(v.Errors, "The release check record is unreadable: "+errText(err))
	}
	if v.Plan, err = rollout.LoadPlan(ctx, s.store); err != nil {
		v.Errors = append(v.Errors, errText(err))
	}
	reports, err := rollout.ListReports(ctx, s.store)
	if err != nil {
		v.Errors = append(v.Errors, "Node versions are unavailable: "+errText(err))
	}
	if rep, err := s.getClusterReport(ctx); err == nil {
		for _, n := range rep.Nodes {
			v.Nodes = append(v.Nodes, upgradeNode{ID: n.ID, Report: reports[n.ID], Current: v.Plan != nil && v.Plan.Current == n.ID})
		}
	} else {
		v.Errors = append(v.Errors, "The node list is unavailable: "+errText(err))
	}
	v.Running = v.Plan != nil && (v.Plan.State == rollout.Running || v.Plan.State == rollout.Paused || v.Plan.State == rollout.Halted)
	v.Offer = v.Latest.Version
	return v
}

func (s *Server) handleUpgrade(w http.ResponseWriter, r *http.Request) {
	if s.clusterUnavailable(w, r) {
		return
	}
	s.render(w, http.StatusOK, "upgrade.html", upgradeData{
		Page: s.newPage(w, r, "Upgrades", "upgrade", crumb{Label: "Upgrades"}),
		View: s.loadUpgrade(r.Context()),
	})
}

// handleUpgradeEvents re-renders the page body on every write under /upgrade/.
func (s *Server) handleUpgradeEvents(w http.ResponseWriter, r *http.Request) {
	if s.clusterUnavailable(w, r) {
		return
	}
	changes, err := fanInWatches(r.Context(), s.store, store.Key("/upgrade/"))
	if err != nil {
		http.Error(w, "live updates unavailable", http.StatusServiceUnavailable)
		return
	}
	csrf := sessionFromContext(r.Context()).CSRFToken
	streamFragments(w, r, "upgrade", changes, 0, func(ctx context.Context) (string, error) {
		return s.fragment("upgrade-fragment", upgradeFragment{View: s.loadUpgrade(ctx), CSRF: csrf})
	})
}

func (s *Server) handleUpgradeStart(w http.ResponseWriter, r *http.Request) {
	if s.clusterUnavailable(w, r) {
		return
	}
	version := strings.TrimPrefix(strings.TrimSpace(r.PostFormValue("version")), "v")
	if !upgrade.ValidVersion(version) {
		s.renderError(w, r, http.StatusBadRequest, "“"+version+"” is not a release version, such as 1.12.0.")
		return
	}
	ctx := r.Context()
	if p, err := rollout.LoadPlan(ctx, s.store); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, errText(err))
		return
	} else if p != nil && p.State != rollout.Done && p.State != rollout.Cancelled {
		s.renderError(w, r, http.StatusConflict, "A rollout to "+p.Target+" is already "+string(p.State)+". Resume or cancel it first.")
		return
	}
	rep, err := s.getClusterReport(ctx)
	if err != nil {
		s.renderError(w, r, http.StatusServiceUnavailable, errText(err))
		return
	}
	var nodes []string
	leader := ""
	for _, n := range rep.Nodes {
		nodes = append(nodes, n.ID)
		if isLeader(n) {
			leader = n.ID
		}
	}
	now := time.Now()
	plan := rollout.Plan{Target: version, State: rollout.Running, Order: rollout.Order(nodes, leader), Started: now, Updated: now}
	if err := rollout.SavePlan(ctx, s.store, plan); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, errText(err))
		return
	}
	s.done(w, r, "success", "Upgrading the cluster to "+version+", one node at a time.", "/upgrade")
}

// planChange applies change to the stored plan; from, when set, is the state it must be in.
func (s *Server) planChange(from rollout.State, change func(*rollout.Plan), msg string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		p, err := rollout.LoadPlan(ctx, s.store)
		if err != nil || p == nil || (from != "" && p.State != from) || p.State == rollout.Done || p.State == rollout.Cancelled {
			s.renderError(w, r, http.StatusConflict, "There is no rollout in a state to do that.")
			return
		}
		change(p)
		p.Updated = time.Now()
		if err := rollout.SavePlan(ctx, s.store, *p); err != nil {
			s.renderError(w, r, http.StatusInternalServerError, errText(err))
			return
		}
		s.done(w, r, "success", msg, "/upgrade")
	})
}

func (s *Server) pausePlan(p *rollout.Plan) { p.State = rollout.Paused }

// resumePlan restarts the clock, so a node whose upgrade failed is tried again.
func (s *Server) resumePlan(p *rollout.Plan) {
	p.State, p.Reason, p.Started = rollout.Running, "", time.Now()
}

func (s *Server) cancelPlan(p *rollout.Plan) { p.State, p.Current = rollout.Cancelled, "" }

func (s *Server) handleUpgradeCheck(w http.ResponseWriter, r *http.Request) {
	s.updateLatest(w, r, func(l *rollout.Latest) { l.Requested = true }, "Checking for a new release.")
}

func (s *Server) handleUpgradeAutoCheck(w http.ResponseWriter, r *http.Request) {
	on := r.PostFormValue("enabled") == "on"
	msg := "The daily check for new releases is off."
	if on {
		msg = "The cluster checks for a new release once a day."
	}
	s.updateLatest(w, r, func(l *rollout.Latest) { l.Disabled = !on }, msg)
}

func (s *Server) updateLatest(w http.ResponseWriter, r *http.Request, change func(*rollout.Latest), msg string) {
	l, err := rollout.LoadLatest(r.Context(), s.store)
	if err == nil {
		change(&l)
		err = rollout.SaveLatest(r.Context(), s.store, l)
	}
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, errText(err))
		return
	}
	s.done(w, r, "success", msg, "/upgrade")
}

// upgradeFragment is what the live part of the page renders from.
type upgradeFragment struct {
	View upgradeView
	CSRF string
}
