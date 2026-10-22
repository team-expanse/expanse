// Nodes: the cluster-wide list (from the same control.Report the
// cluster page reads, plus each agent's /nodes/<id>/status heartbeat)
// and a per-node detail page. Inventory, health checks and reconciler
// status come from the in-process NodeService, which only knows this
// node, so they render for the serving node and are labelled as such
// for any other.
package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/expanse/expanse/internal/cluster/control"
	"github.com/expanse/expanse/internal/cluster/join"
	"github.com/expanse/expanse/internal/config"
	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
)

func (s *Server) registerNodeRoutes() {
	s.routes.Handle("GET /nodes", s.requireAuth(http.HandlerFunc(s.handleNodesList)))
	s.routes.Handle("GET /nodes/events", s.requireAuth(http.HandlerFunc(s.handleNodesEvents)))
	s.routes.Handle("GET /nodes/{id}", s.requireAuth(http.HandlerFunc(s.handleNodeDetail)))
	s.routes.Handle("GET /nodes/{id}/events", s.requireAuth(http.HandlerFunc(s.handleNodeDetailEvents)))
	s.routes.Handle("POST /nodes/{id}/storage", s.requireAuth(http.HandlerFunc(s.handleNodeStorageEnable)))
	s.routes.Handle("POST /nodes/{id}/generations", s.requireAuth(http.HandlerFunc(s.handleNodeGenerationsClean)))
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
	changes, err := s.watchClusterPrefixes(r.Context())
	if err != nil {
		http.Error(w, "live updates unavailable", http.StatusServiceUnavailable)
		return
	}
	streamFragments(w, r, "nodes", changes, 0, func(ctx context.Context) (string, error) {
		nodes, rep, err := s.loadNodes(ctx)
		if err != nil {
			return "", nil //nolint:nilerr // transient; the next write retries
		}
		return s.fragment("nodes-fragment", nodesListData{Nodes: nodes, Report: rep})
	})
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
	Storage        nodeStorageView
	Generations    nodeGenerationsView
	// Tab is the section shown: "" (overview), "hardware", "checks" or "generations".
	Tab           string
	Replicas      []nodeReplicaView
	ReplicasError string
}

// nodeReplicaView is one block replica running on the node.
type nodeReplicaView struct {
	Block string // namespace/name
	Index int32
	Phase pill
	Ready pill
}

// nodeReplicas lists the live placements on node id, in block order.
func nodeReplicas(blocks []*pb.Block, id string) []nodeReplicaView {
	var out []nodeReplicaView
	for _, b := range blocks {
		for _, p := range b.GetStatus().GetPlacements() {
			if p.GetNodeId() != id || p.GetReplicaIndex() < 0 {
				continue
			}
			out = append(out, nodeReplicaView{
				Block: b.GetMetadata().GetNamespace() + "/" + b.GetMetadata().GetName(),
				Index: p.GetReplicaIndex(), Phase: phasePill(p.GetPhase()), Ready: replicaRow(p).Ready,
			})
		}
	}
	return out
}

// nodeTab is the ?tab= section, defaulting to the overview.
func nodeTab(r *http.Request) string {
	switch t := r.URL.Query().Get("tab"); t {
	case "hardware", "checks", "generations":
		return t
	}
	return ""
}

func (s *Server) handleNodeDetail(w http.ResponseWriter, r *http.Request) {
	if data, ok := s.nodeDetail(w, r); ok {
		s.render(w, http.StatusOK, "node_detail.html", data)
	}
}

// nodeDetail loads the page for the {id} node; when ok is false it has answered already.
func (s *Server) nodeDetail(w http.ResponseWriter, r *http.Request) (nodeDetailData, bool) {
	if s.clusterUnavailable(w, r) {
		return nodeDetailData{}, false
	}
	id := r.PathValue("id")
	data, found, err := s.loadNodeDetail(r.Context(), id)
	switch {
	case err != nil:
		s.renderError(w, r, http.StatusServiceUnavailable, errText(err))
		return nodeDetailData{}, false
	case !found:
		s.renderError(w, r, http.StatusNotFound, "There is no node named "+id+" in this cluster.")
		return nodeDetailData{}, false
	}
	name := s.names.name(id)
	data.Page = s.newPage(w, r, name, "nodes", crumb{Label: "Nodes", Href: "/nodes"}, crumb{Label: name})
	data.Tab = nodeTab(r)
	return data, true
}

func (s *Server) loadNodeDetail(ctx context.Context, id string) (data nodeDetailData, found bool, err error) {
	nodes, _, err := s.loadNodes(ctx)
	if err != nil {
		return data, false, err
	}
	for _, n := range nodes {
		if n.ID == id {
			data.Node, found = n, true
		}
	}
	if !found {
		return data, false, nil
	}
	if data.Node.Serving {
		s.loadLocalNodeDetail(ctx, &data)
	}
	data.Storage = s.nodeStorageView(ctx, id)
	data.Generations = s.nodeGenerationsView(ctx, id)
	s.loadNodeReplicas(ctx, &data)
	return data, true, nil
}

func (s *Server) loadNodeReplicas(ctx context.Context, data *nodeDetailData) {
	if s.blocks == nil {
		data.ReplicasError = "Blocks are not served on this node."
		return
	}
	resp, err := s.blocks.List(store.WithStale(ctx), &pb.ListBlocksRequest{})
	if err != nil {
		data.ReplicasError = "Block placements are unavailable: " + errText(err)
		return
	}
	data.Replicas = nodeReplicas(resp.GetBlocks(), data.Node.ID)
}

// handleNodeDetailEvents streams the node page. Store writes cover its
// record and storage; the tick covers the agent's local inventory and checks.
func (s *Server) handleNodeDetailEvents(w http.ResponseWriter, r *http.Request) {
	if s.clusterUnavailable(w, r) {
		return
	}
	id := r.PathValue("id")
	changes, err := fanInWatches(r.Context(), s.store, store.Key(join.NodesKeyPrefix), store.Key("/cluster/"), store.Key(storage.NodeStoragePrefix), store.Key("/node/"+id+"/"))
	if err != nil {
		http.Error(w, "live updates unavailable", http.StatusServiceUnavailable)
		return
	}
	pg, tab := page{CSRFToken: sessionFromContext(r.Context()).CSRFToken}, nodeTab(r)
	streamEvents(w, r, changes, healthTick, func(ctx context.Context) ([]sseEvent, error) {
		data, found, err := s.loadNodeDetail(ctx, id)
		if err != nil || !found {
			return nil, nil //nolint:nilerr // transient, or the node left; the next change retries
		}
		data.Page, data.Tab = pg, tab
		data.Page.NodeName = s.nodeName(ctx)
		var events []sseEvent
		// Separate regions, so a tick's redraw leaves the storage form alone.
		for _, reg := range [][2]string{{"node", "node-fragment"}, {"node-storage", "node-storage"}, {"node-local", "node-local"}, {"node-generations", "node-generations"}} {
			frag, err := s.fragment(reg[1], data)
			if err != nil {
				return nil, err
			}
			events = append(events, sseEvent{reg[0], frag})
		}
		return events, nil
	})
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
