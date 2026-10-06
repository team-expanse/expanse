// Stream D (PHASE-09-TASKS.md X4): the in-cluster health/alert view --
// node checks, resource (including block) health, volume health and
// quorum, the exact same signals internal/metrics exports to Prometheus
// (D3), surfaced live over the existing SSE pattern (Stream B1) so an
// operator sees degradation without any external Prometheus/Grafana
// configured. Alerts mirror deploy/prometheus/expanse-alerts.rules.yml's
// own immediate (`for: 0s`) critical conditions -- one definition of
// "critical", reused, not a second one invented here.
package web

import (
	"bytes"
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/expanse/expanse/internal/cluster/control"
	"github.com/expanse/expanse/internal/cluster/join"
	"github.com/expanse/expanse/internal/cluster/nodelc"
	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
)

func (s *Server) registerHealthRoutes() {
	s.routes.Handle("GET /health", s.requireAuth(http.HandlerFunc(s.handleHealthOverview)))
	s.routes.Handle("GET /health/events", s.requireAuth(http.HandlerFunc(s.handleHealthEvents)))
}

type nodeCheckView struct {
	Name, Message string
	Status        pill
}

type resourceView struct {
	Type, ID string
	Status   pill
}

// alertView is one currently-firing critical condition, named after the
// matching rule in deploy/prometheus/expanse-alerts.rules.yml so an
// operator sees the identical name here and in Alertmanager.
type alertView struct {
	Name, Summary string
}

type healthOverviewData struct {
	Page       page
	NodeChecks []nodeCheckView
	Resources  []resourceView
	Volumes    []*volumeView
	Report     *control.Report
	Alerts     []alertView
	Error      string
}

// loadHealthOverview gathers the same four signal categories
// internal/metrics.Collector exports (node checks, resource health,
// volume health, quorum), all read stale (R5's fix, mirrored here): a
// genuinely degraded cluster is exactly when this page must keep
// rendering, not block on a 5s leader-wait timeout.
func (s *Server) loadHealthOverview(ctx context.Context) (healthOverviewData, error) {
	var data healthOverviewData

	var checks []*pb.CheckResult
	if rep, err := s.cluster.GetHealth(ctx, &pb.GetHealthRequest{}); err == nil {
		checks = rep.GetChecks()
	}
	// A node whose checks haven't run yet ("health not collected yet")
	// renders with none, not a page error -- the same transient
	// condition collectNodeHealth already tolerates.
	for _, c := range checks {
		data.NodeChecks = append(data.NodeChecks, nodeCheckView{
			Name: c.GetName(), Status: healthPill(c.GetStatus()), Message: c.GetMessage(),
		})
	}

	resResp, err := s.cluster.ListResources(store.WithStale(ctx), &pb.ListResourcesRequest{})
	if err != nil {
		return data, err
	}
	var resources []*pb.Resource
	for _, r := range resResp.GetResources() {
		resources = append(resources, r)
		data.Resources = append(data.Resources, resourceView{Type: r.GetType(), ID: r.GetId(), Status: healthPill(r.GetHealth())})
	}

	ids, err := storage.ListVolumeIDs(store.WithStale(ctx), s.store)
	if err != nil {
		return data, err
	}
	for _, id := range ids {
		v, err := s.loadVolumeView(store.WithStale(ctx), id)
		if err != nil {
			continue // mid-delete/creation; skip rather than fail the whole page (volumes.go's own convention)
		}
		data.Volumes = append(data.Volumes, v)
	}
	sort.Slice(data.Volumes, func(i, j int) bool { return data.Volumes[i].Name < data.Volumes[j].Name })

	report, err := s.getClusterReport(ctx)
	if err != nil {
		return data, err
	}
	data.Report = report

	data.Alerts = buildAlerts(checks, resources, data.Volumes, report)
	return data, nil
}

// buildAlerts derives the currently-firing critical conditions directly
// from the same signals above, using the identical thresholds as
// deploy/prometheus/expanse-alerts.rules.yml's seven `for: 0s` rules --
// the warning-severity rules (which wait out a `for` window to avoid
// flapping) are left to Prometheus/Alertmanager, not reimplemented here.
func buildAlerts(checks []*pb.CheckResult, resources []*pb.Resource, volumes []*volumeView, report *control.Report) []alertView {
	var alerts []alertView
	for _, c := range checks {
		if c.GetStatus() == pb.Health_HEALTH_UNHEALTHY {
			summary := "check " + c.GetName() + " is unhealthy"
			if m := c.GetMessage(); m != "" {
				summary += ": " + m
			}
			alerts = append(alerts, alertView{Name: "ExpanseNodeUnhealthy", Summary: summary})
		}
	}
	for _, r := range resources {
		if r.GetHealth() == pb.Health_HEALTH_UNHEALTHY {
			alerts = append(alerts, alertView{Name: "ExpanseResourceUnhealthy", Summary: r.GetType() + " " + r.GetId() + " is unhealthy"})
		}
	}
	for _, v := range volumes {
		switch v.State {
		case storage.StateFailed, storage.StateNeedsManualRecovery:
			alerts = append(alerts, alertView{Name: "ExpanseVolumeFailed", Summary: "volume " + v.Name + " has failed (" + string(v.State) + ")"})
		case storage.StateReadOnly:
			alerts = append(alerts, alertView{Name: "ExpanseVolumeReadOnly", Summary: "volume " + v.Name + " is read-only"})
		}
	}
	if report != nil {
		if report.Leader == "" {
			alerts = append(alerts, alertView{Name: "ExpanseQuorumNoLeader", Summary: "no Raft leader -- the cluster is read-only"})
		}
		if report.Degraded {
			alerts = append(alerts, alertView{Name: "ExpanseQuorumDegraded", Summary: "this node cannot reach Raft quorum"})
		}
		// X5's own case: a node that has gone silent without the
		// cluster losing quorum or the node self-reporting anything --
		// only nodelc's leader-side detector (§4.8) notices this.
		for _, n := range report.Nodes {
			if n.Lifecycle == nodelc.StateUnreachable || n.Lifecycle == nodelc.StateFailed {
				alerts = append(alerts, alertView{Name: "ExpanseNodeUnreachable", Summary: "node " + n.ID + " is unreachable"})
			}
		}
	}
	return alerts
}

func (s *Server) handleHealthOverview(w http.ResponseWriter, r *http.Request) {
	if s.clusterUnavailable(w, r) {
		return
	}
	data, err := s.loadHealthOverview(r.Context())
	data.Page = s.newPage(w, r, "Health & alerts", "health", crumb{Label: "Health"})
	if err != nil {
		data.Error = errText(err)
	}
	s.render(w, http.StatusOK, "health_overview.html", data)
}

// handleHealthEvents streams the live health/alert view as SSE, the same
// "push, never poll" pattern as /cluster and /blocks/.../events. Store
// writes under this node's resources, the volume tree, node records and
// cluster meta all trigger an immediate re-render via fanInWatches; a
// short ticker additionally covers node health checks, which have no
// store-backed representation to watch (Collect's own comment: "a live
// local snapshot, no store I/O") -- well under D6's slowest
// scrape-interval target (10-15s), so it cannot itself threaten the 30s
// alert-visibility budget.
func (s *Server) handleHealthEvents(w http.ResponseWriter, r *http.Request) {
	if s.clusterUnavailable(w, r) {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ctx := r.Context()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush() // confirm the stream is live before the first event

	send := func() error {
		data, err := s.loadHealthOverview(ctx)
		if err != nil {
			return nil //nolint:nilerr // transient read error; the next tick/event retries
		}
		var buf bytes.Buffer
		if err := s.renderFragment(&buf, "health-fragment", data); err != nil {
			return err
		}
		if err := writeSSE(w, "health", buf.String()); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	// Watch before the initial snapshot (B1's fix, applied from the
	// start): a write landing between a snapshot read and the watch's
	// starting revision would otherwise be delivered by neither.
	changes, err := fanInWatches(ctx, s.store,
		store.Key("/node/"+s.NodeID+"/"), store.Key(storage.VolumePrefix),
		store.Key(join.NodesKeyPrefix), store.Key("/cluster/"),
	)
	if err != nil {
		return
	}
	if err := send(); err != nil {
		return
	}

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case _, ok := <-changes:
			if !ok {
				changes = nil
				continue
			}
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
