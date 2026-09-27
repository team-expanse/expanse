// Nodes: the cluster-wide list (from the same control.Report the
// cluster page reads, plus each agent's /nodes/<id>/status heartbeat)
// and a per-node detail page. Inventory, health checks and reconciler
// status come from the in-process NodeService, which only knows this
// node, so they render for the serving node and are labelled as such
// for any other.
package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/expanse/expanse/internal/cluster/control"
	"github.com/expanse/expanse/internal/cluster/join"
	"github.com/expanse/expanse/internal/config"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
)

func (s *Server) registerNodeRoutes() {
	s.routes.Handle("GET /nodes", s.requireAuth(http.HandlerFunc(s.handleNodesList)))
	s.routes.Handle("GET /nodes/events", s.requireAuth(http.HandlerFunc(s.handleNodesEvents)))
	s.routes.Handle("GET /nodes/{id}", s.requireAuth(http.HandlerFunc(s.handleNodeDetail)))
}

// nodeView is one node as the list and detail pages show it.
type nodeView struct {
	control.NodeStatus
	State    pill
	Health   pill
	Leader   bool
	JoinedAt time.Time
	Serving  bool
	// UIURL is a best-effort link to the UI served by that node itself,
	// derived from its API address and the fixed UI port.
	UIURL string
}

// loadNodes joins the report's node table with each node's record and
// status heartbeat, all read stale so a degraded node still lists.
func (s *Server) loadNodes(ctx context.Context) ([]nodeView, *control.Report, error) {
	ctx = store.WithStale(ctx)
	rep, err := s.getClusterReport(ctx)
	if err != nil {
		return nil, nil, err
	}
	var views []nodeView
	for _, n := range rep.Nodes {
		v := nodeView{NodeStatus: n, State: nodePill(n), Leader: isLeader(n), Serving: n.ID == s.NodeID}
		v.Health = heartbeatPill(n, "")
		if e, err := s.store.Get(ctx, store.Key(join.NodesKeyPrefix+n.ID+"/status")); err == nil {
			v.Health = heartbeatPill(n, string(e.Value))
		}
		if e, err := s.store.Get(ctx, store.Key(join.NodesKeyPrefix+n.ID)); err == nil {
			var rec join.NodeRecord
			if json.Unmarshal(e.Value, &rec) == nil && rec.JoinedAt > 0 {
				v.JoinedAt = time.Unix(0, rec.JoinedAt)
			}
		}
		v.UIURL = nodeUIURL(n)
		views = append(views, v)
	}
	return views, rep, nil
}

// nodeUIURL links to the node's own UI via its API host, else its Raft host (API addresses may omit the host).
func nodeUIURL(n control.NodeStatus) string {
	for _, addr := range []string{n.APIAddr, n.RaftAddr} {
		if host, _, err := net.SplitHostPort(addr); err == nil && host != "" {
			return fmt.Sprintf("https://%s/nodes/%s", net.JoinHostPort(host, fmt.Sprint(config.PortUI)), n.ID)
		}
	}
	return ""
}

type nodesListData struct {
	Page   page
	Nodes  []nodeView
	Report *control.Report
	Error  string
}

func (s *Server) handleNodesList(w http.ResponseWriter, r *http.Request) {
	if s.clusterUnavailable(w, r) {
		return
	}
	data := nodesListData{Page: s.newPage(w, r, "Nodes", "nodes", crumb{Label: "Nodes"})}
	nodes, rep, err := s.loadNodes(r.Context())
	if err != nil {
		data.Error = errText(err)
	}
	data.Nodes, data.Report = nodes, rep
	s.render(w, http.StatusOK, "nodes_list.html", data)
}

// handleNodesEvents pushes the node table on every node record/status
// or cluster-meta write, the same prefixes the cluster page watches.
func (s *Server) handleNodesEvents(w http.ResponseWriter, r *http.Request) {
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
	flusher.Flush()

	send := func() error {
		nodes, rep, err := s.loadNodes(r.Context())
		if err != nil {
			return nil // transient; the next write retries
		}
		var buf bytes.Buffer
		if err := s.renderFragment(&buf, "nodes-fragment", nodesListData{Nodes: nodes, Report: rep}); err != nil {
			return err
		}
		if err := writeSSE(w, "nodes", buf.String()); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
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

type nodeDetailData struct {
	Page page
	Node nodeView
	// Local-only sections, populated for the serving node.
	Inventory      *pb.Inventory
	InventoryError string
	Checks         []nodeCheckView
	ChecksError    string
	Status         *pb.NodeStatus
	StatusPill     pill
	Resources      []resourceView
}

func (s *Server) handleNodeDetail(w http.ResponseWriter, r *http.Request) {
	if s.clusterUnavailable(w, r) {
		return
	}
	id := r.PathValue("id")
	nodes, _, err := s.loadNodes(r.Context())
	if err != nil {
		s.renderError(w, r, http.StatusServiceUnavailable, errText(err))
		return
	}
	data := nodeDetailData{Page: s.newPage(w, r, id, "nodes", crumb{Label: "Nodes", Href: "/nodes"}, crumb{Label: id})}
	found := false
	for _, n := range nodes {
		if n.ID == id {
			data.Node, found = n, true
		}
	}
	if !found {
		s.renderError(w, r, http.StatusNotFound, "There is no node named "+id+" in this cluster.")
		return
	}
	if data.Node.Serving {
		s.loadLocalNodeDetail(r.Context(), &data)
	}
	s.render(w, http.StatusOK, "node_detail.html", data)
}

// loadLocalNodeDetail fills the sections only the in-process
// NodeService can answer, each independently tolerant of "not yet".
func (s *Server) loadLocalNodeDetail(ctx context.Context, data *nodeDetailData) {
	if inv, err := s.cluster.GetInventory(ctx, &pb.GetInventoryRequest{}); err == nil {
		data.Inventory = inv
	} else {
		data.InventoryError = "Inventory not collected yet; the agent gathers it shortly after start."
	}
	if rep, err := s.cluster.GetHealth(ctx, &pb.GetHealthRequest{}); err == nil {
		for _, c := range rep.GetChecks() {
			data.Checks = append(data.Checks, nodeCheckView{Name: c.GetName(), Status: healthPill(c.GetStatus()), Message: c.GetMessage()})
		}
	} else {
		data.ChecksError = "Health checks have not run yet."
	}
	if st, err := s.cluster.GetStatus(ctx, &pb.GetStatusRequest{}); err == nil {
		data.Status, data.StatusPill = st, agentPill(st.GetStatus())
	}
	if rr, err := s.cluster.ListResources(store.WithStale(ctx), &pb.ListResourcesRequest{}); err == nil {
		for _, res := range rr.GetResources() {
			data.Resources = append(data.Resources, resourceView{Type: res.GetType(), ID: res.GetId(), Status: healthPill(res.GetHealth())})
		}
	}
}
