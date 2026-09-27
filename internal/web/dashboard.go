// The dashboard at "/": one screen with the cluster's quorum, nodes,
// blocks by phase, volumes by state, firing alerts and recent events,
// pushed live over the same SSE pattern the other pages use. It reads
// the same sources those pages read, never a second derivation.
package web

import (
	"bytes"
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/expanse/expanse/internal/cluster/control"
	"github.com/expanse/expanse/internal/cluster/nodelc"
	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
)

func (s *Server) registerDashboardRoutes() {
	s.routes.Handle("GET /dashboard/events", s.requireAuth(http.HandlerFunc(s.handleDashboardEvents)))
}

// dashboardData is the fragment's data; every section tolerates its
// source being unavailable (nil Report, no block API) so a partial
// cluster still paints a partial dashboard.
type dashboardData struct {
	Page       page
	Report     *control.Report
	NodesUp    int
	NodesTotal int
	NodesDown  []control.NodeStatus
	Blocks     phaseCounts
	BlocksAPI  bool
	Unhealthy  []*pb.Block
	Volumes    volumeCounts
	Degraded   []*volumeView
	Alerts     []alertView
	Events     []eventEntry
	Standalone bool
}

func (s *Server) loadDashboard(ctx context.Context) dashboardData {
	ctx = store.WithStale(ctx)
	d := dashboardData{Standalone: s.cluster == nil, Events: s.events.recent("", 8)}
	if s.cluster != nil {
		if rep, err := s.getClusterReport(ctx); err == nil {
			d.Report = rep
			d.NodesTotal = len(rep.Nodes)
			for _, n := range rep.Nodes {
				if n.Lifecycle == nodelc.StateUnreachable || n.Lifecycle == nodelc.StateFailed {
					d.NodesDown = append(d.NodesDown, n)
				} else {
					d.NodesUp++
				}
			}
		}
	}
	if s.blocks != nil {
		d.BlocksAPI = true
		if resp, err := s.blocks.List(ctx, &pb.ListBlocksRequest{}); err == nil {
			d.Blocks = countPhases(resp.GetBlocks())
			for _, b := range resp.GetBlocks() {
				if t := phasePill(b.GetStatus().GetPhase()).Tone; t == "warn" || t == "crit" {
					d.Unhealthy = append(d.Unhealthy, b)
				}
			}
		}
	}
	vols := s.loadVolumeViews(ctx)
	d.Volumes = countVolumes(vols)
	for _, v := range vols {
		if t := volumePill(v.State).Tone; t == "warn" || t == "crit" {
			d.Degraded = append(d.Degraded, v)
		}
	}
	if s.cluster != nil {
		d.Alerts = s.currentAlerts(ctx, vols, d.Report)
	}
	return d
}

// loadVolumeViews lists every volume the same way the volumes page does,
// skipping half-written records.
func (s *Server) loadVolumeViews(ctx context.Context) []*volumeView {
	ids, err := storage.ListVolumeIDs(ctx, s.store)
	if err != nil {
		return nil
	}
	var views []*volumeView
	for _, id := range ids {
		if v, err := s.loadVolumeView(ctx, id); err == nil {
			views = append(views, v)
		}
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })
	return views
}

// currentAlerts derives the firing alerts from the same signals the
// health page uses (buildAlerts), tolerating each source's absence.
func (s *Server) currentAlerts(ctx context.Context, vols []*volumeView, rep *control.Report) []alertView {
	var checks []*pb.CheckResult
	if h, err := s.cluster.GetHealth(ctx, &pb.GetHealthRequest{}); err == nil {
		checks = h.GetChecks()
	}
	var resources []*pb.Resource
	if rr, err := s.cluster.ListResources(ctx, &pb.ListResourcesRequest{}); err == nil {
		resources = rr.GetResources()
	}
	return buildAlerts(checks, resources, vols, rep)
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	d := s.loadDashboard(r.Context())
	d.Page = s.newPage(w, r, "Dashboard", "dashboard")
	s.render(w, http.StatusOK, "dashboard.html", d)
}

// handleDashboardEvents re-renders the dashboard fragment on any store
// write (coalesced, since a reconcile tick is a burst) and every few
// seconds for the check-driven alerts that have no store footprint.
func (s *Server) handleDashboardEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ctx := r.Context()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	send := func() error {
		var buf bytes.Buffer
		if err := s.renderFragment(&buf, "dashboard-fragment", s.loadDashboard(ctx)); err != nil {
			return err
		}
		if err := writeSSE(w, "dashboard", buf.String()); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	changes, err := fanInWatches(ctx, s.store, store.Key("/"))
	if err != nil {
		return
	}
	if err := send(); err != nil {
		return
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case _, ok := <-changes:
			if !ok {
				changes = nil
				continue
			}
			coalesce(ctx, changes, 250*time.Millisecond)
			if err := send(); err != nil {
				return
			}
		case <-ticker.C:
			if err := send(); err != nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// coalesce drains further change signals for a short window so a burst
// of writes produces one re-render, not one per write.
func coalesce(ctx context.Context, changes <-chan struct{}, window time.Duration) {
	deadline := time.After(window)
	for {
		select {
		case _, ok := <-changes:
			if !ok {
				return
			}
		case <-deadline:
			return
		case <-ctx.Done():
			return
		}
	}
}
