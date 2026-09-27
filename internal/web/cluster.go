// Stream B1: the cluster overview page — nodes, quorum, leader,
// generation, all pushed live over SSE (X4), never polled. It calls the
// same in-process NodeService.GetClusterStatus the local gRPC socket
// serves (D1), reusing control.Status's degraded/stale-read fallback
// rather than re-deriving the report here.
package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"github.com/expanse/expanse/internal/cluster/control"
	"github.com/expanse/expanse/internal/cluster/join"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
)

func (s *Server) registerClusterRoutes() {
	s.routes.Handle("GET /cluster", s.requireAuth(http.HandlerFunc(s.handleClusterOverview)))
	s.routes.Handle("GET /cluster/events", s.requireAuth(http.HandlerFunc(s.handleClusterEvents)))
}

// clusterUnavailable reports no NodeService being wired on this node
// (e.g. a non-cluster agent, or a test server that didn't set one up)
// rather than panicking on a nil s.cluster.
func (s *Server) clusterUnavailable(w http.ResponseWriter, r *http.Request) bool {
	if s.cluster != nil {
		return false
	}
	s.renderError(w, r, http.StatusServiceUnavailable, "Cluster status is not available on this node: it is not a cluster member.")
	return true
}

// getClusterReport calls the in-process NodeService and decodes its
// JSON payload back into the same control.Report type GetClusterStatus
// marshaled it from.
func (s *Server) getClusterReport(ctx context.Context) (*control.Report, error) {
	resp, err := s.cluster.GetClusterStatus(ctx, &pb.GetClusterStatusRequest{})
	if err != nil {
		return nil, err
	}
	var rep control.Report
	if err := json.Unmarshal(resp.GetReportJson(), &rep); err != nil {
		return nil, err
	}
	return &rep, nil
}

type clusterOverviewData struct {
	Page   page
	Report *control.Report
	Error  string
}

func (s *Server) handleClusterOverview(w http.ResponseWriter, r *http.Request) {
	if s.clusterUnavailable(w, r) {
		return
	}
	data := clusterOverviewData{Page: s.newPage(w, r, "Cluster", "cluster", crumb{Label: "Cluster"})}
	rep, err := s.getClusterReport(r.Context())
	if err != nil {
		data.Error = errText(err)
	} else {
		data.Report = rep
	}
	s.render(w, http.StatusOK, "cluster_overview.html", data)
}

// handleClusterEvents streams the live overview as SSE (X4): every
// change under the prefixes the report reads (node join/leave/status,
// cluster meta/generation) triggers a full re-render, pushed as one
// "cluster-fragment" swap — no client polling.
func (s *Server) handleClusterEvents(w http.ResponseWriter, r *http.Request) {
	if s.clusterUnavailable(w, r) {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush() // confirm the stream is live before the first event

	send := func() error {
		rep, err := s.getClusterReport(r.Context())
		if err != nil {
			return nil // transient read error; the next event retries
		}
		var buf bytes.Buffer
		if err := s.renderFragment(&buf, "cluster-fragment", rep); err != nil {
			return err
		}
		if err := writeSSE(w, "cluster", buf.String()); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	// Start the watch BEFORE the initial snapshot, not after: a write
	// landing between "read the snapshot" and "start watching from the
	// current revision" would otherwise be silently missed until some
	// later, unrelated write happened to wake delivery again. Watching
	// first means the snapshot may occasionally be one write stale, but
	// every write is then guaranteed a follow-up event, never dropped.
	changes, err := s.watchClusterPrefixes(r.Context())
	if err != nil {
		return
	}
	if err := send(); err != nil {
		return
	}
	for range changes {
		if err := send(); err != nil {
			return
		}
	}
}

// watchClusterPrefixes fans in store events from every prefix
// control.Status reads (node records/status under join.NodesKeyPrefix,
// meta/generation under "/cluster/") into one channel, so the overview
// refreshes on exactly the writes that could change it.
func (s *Server) watchClusterPrefixes(ctx context.Context) (<-chan struct{}, error) {
	return fanInWatches(ctx, s.store, store.Key(join.NodesKeyPrefix), store.Key("/cluster/"))
}
