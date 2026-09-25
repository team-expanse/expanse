package metrics

import (
	"context"
	"encoding/json"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/expanse/expanse/internal/cluster/control"
	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
)

// Health metric values reuse pb.Health's own enum ordinals directly
// (HEALTHY=1, DEGRADED=2, UNHEALTHY=3, UNKNOWN=4) rather than inventing a
// second numbering -- one canonical mapping, already the wire format
// every RPC client in this project decodes.
var (
	nodeHealthDesc = prometheus.NewDesc(
		"expanse_node_health",
		"This node's own health check result (pb.Health ordinal: 1=healthy 2=degraded 3=unhealthy 4=unknown).",
		[]string{"node", "check", "status"}, nil)
	resourceHealthDesc = prometheus.NewDesc(
		"expanse_resource_health",
		"A reconciled resource's health, including blocks (pb.Health ordinal, see expanse_node_health).",
		[]string{"type", "id", "status"}, nil)
	volumeHealthDesc = prometheus.NewDesc(
		"expanse_volume_health",
		"A volume's lifecycle state (pb.VolumeState ordinal: 2=healthy 3=degraded 4=readonly 5=resyncing 6=failed 8=needs-manual-recovery).",
		[]string{"volume", "state"}, nil)
	quorumVotersDesc       = prometheus.NewDesc("expanse_quorum_voters", "Raft voters this node currently sees.", nil, nil)
	quorumNeedDesc         = prometheus.NewDesc("expanse_quorum_need", "Raft voters required for quorum.", nil, nil)
	quorumHasLeaderDesc    = prometheus.NewDesc("expanse_quorum_has_leader", "1 if this node's report has a leader, else 0.", nil, nil)
	quorumDegradedDesc     = prometheus.NewDesc("expanse_quorum_degraded", "1 if the cluster is degraded (no leader, read-only), else 0.", nil, nil)
	quorumNodeDegradedDesc = prometheus.NewDesc(
		"expanse_quorum_node_degraded",
		"1 if a member node self-reports degraded in the quorum report, else 0 (D5's single liveness signal).",
		[]string{"node"}, nil)
)

// Collector exports this project's existing health signals as
// Prometheus metrics. It queries the same in-process NodeService (D... )
// internal/web already calls for its own live UI (Stream B1 precedent)
// and internal/storage's volume records directly -- no second
// health-computation path, only translation.
type Collector struct {
	node  pb.NodeServiceServer
	store store.Store
}

// NewCollector builds a Collector over the node's own already-wired
// NodeService implementation and store -- the same objects agent.go
// hands to the gRPC socket and the web UI.
func NewCollector(node pb.NodeServiceServer, st store.Store) *Collector {
	return &Collector{node: node, store: st}
}

func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	// Metric.Desc()s depend on live label values (resource/volume
	// counts), so describe by collecting once, the documented pattern
	// for collectors whose series set isn't known statically.
	prometheus.DescribeByCollect(c, ch)
}

func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	ctx := context.Background()
	c.collectNodeHealth(ctx, ch)
	c.collectResourceHealth(ctx, ch)
	c.collectVolumeHealth(ctx, ch)
	c.collectQuorum(ctx, ch)
}

func (c *Collector) collectNodeHealth(ctx context.Context, ch chan<- prometheus.Metric) {
	rep, err := c.node.GetHealth(ctx, &pb.GetHealthRequest{})
	if err != nil {
		return // health not collected yet; nothing to export this scrape
	}
	for _, check := range rep.GetChecks() {
		ch <- prometheus.MustNewConstMetric(nodeHealthDesc, prometheus.GaugeValue,
			float64(check.GetStatus()), check.GetName(), check.GetName(), check.GetStatus().String())
	}
}

func (c *Collector) collectResourceHealth(ctx context.Context, ch chan<- prometheus.Metric) {
	resp, err := c.node.ListResources(ctx, &pb.ListResourcesRequest{})
	if err != nil {
		return
	}
	for _, r := range resp.GetResources() {
		ch <- prometheus.MustNewConstMetric(resourceHealthDesc, prometheus.GaugeValue,
			float64(r.GetHealth()), r.GetType(), r.GetId(), r.GetHealth().String())
	}
}

func (c *Collector) collectVolumeHealth(ctx context.Context, ch chan<- prometheus.Metric) {
	if c.store == nil {
		return
	}
	ids, err := storage.ListVolumeIDs(ctx, c.store)
	if err != nil {
		return
	}
	for _, id := range ids {
		spec, err := storage.LoadSpec(ctx, c.store, id)
		if err != nil {
			continue
		}
		st, _, err := storage.LoadStatus(ctx, c.store, id)
		if err != nil {
			continue
		}
		name := spec.Name
		if name == "" {
			name = id
		}
		ch <- prometheus.MustNewConstMetric(volumeHealthDesc, prometheus.GaugeValue,
			float64(volumeStateOrdinal(st.State)), name, string(st.State))
	}
}

func (c *Collector) collectQuorum(ctx context.Context, ch chan<- prometheus.Metric) {
	resp, err := c.node.GetClusterStatus(ctx, &pb.GetClusterStatusRequest{})
	if err != nil {
		return // not a cluster-mode agent, or status genuinely unavailable
	}
	var rep control.Report
	if err := json.Unmarshal(resp.GetReportJson(), &rep); err != nil {
		return
	}
	ch <- prometheus.MustNewConstMetric(quorumVotersDesc, prometheus.GaugeValue, float64(rep.QuorumHave))
	ch <- prometheus.MustNewConstMetric(quorumNeedDesc, prometheus.GaugeValue, float64(rep.QuorumNeed))
	ch <- prometheus.MustNewConstMetric(quorumHasLeaderDesc, prometheus.GaugeValue, boolFloat(rep.Leader != ""))
	ch <- prometheus.MustNewConstMetric(quorumDegradedDesc, prometheus.GaugeValue, boolFloat(rep.Degraded))
	for _, n := range rep.Nodes {
		ch <- prometheus.MustNewConstMetric(quorumNodeDegradedDesc, prometheus.GaugeValue, boolFloat(n.Degraded), n.ID)
	}
}

func boolFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// volumeStateOrdinal mirrors proto/storage.proto's VolumeState enum
// values directly (the wire encoding every RPC client already decodes),
// rather than inventing a second numbering local to this package.
func volumeStateOrdinal(s storage.VolumeState) int32 {
	switch s {
	case storage.StateCreating:
		return 1
	case storage.StateHealthy:
		return 2
	case storage.StateDegraded:
		return 3
	case storage.StateReadOnly:
		return 4
	case storage.StateResyncing:
		return 5
	case storage.StateFailed:
		return 6
	case storage.StateDeleting:
		return 7
	case storage.StateNeedsManualRecovery:
		return 8
	default:
		return 0
	}
}
