package metrics

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/expanse/expanse/internal/cluster/control"
	"github.com/expanse/expanse/internal/storage"
	pb "github.com/expanse/expanse/proto"
)

// fakeNode answers exactly the RPCs Collect uses, with fixed responses a
// test can assert against — no real store or reconciler needed to check
// the translation logic in isolation.
type fakeNode struct {
	pb.UnimplementedNodeServiceServer
	health    *pb.HealthReport
	resources *pb.ListResourcesResponse
	cluster   *control.Report
	err       error
}

func (f *fakeNode) GetHealth(context.Context, *pb.GetHealthRequest) (*pb.HealthReport, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.health, nil
}

func (f *fakeNode) ListResources(context.Context, *pb.ListResourcesRequest) (*pb.ListResourcesResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.resources, nil
}

func (f *fakeNode) GetClusterStatus(context.Context, *pb.GetClusterStatusRequest) (*pb.GetClusterStatusResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	b, err := json.Marshal(f.cluster)
	if err != nil {
		return nil, err
	}
	return &pb.GetClusterStatusResponse{ReportJson: b}, nil
}

// collect runs the collector once and returns every emitted metric.
func collect(t *testing.T, c *Collector) []prometheus.Metric {
	t.Helper()
	ch := make(chan prometheus.Metric, 64)
	go func() {
		c.Collect(ch)
		close(ch)
	}()
	var out []prometheus.Metric
	for m := range ch {
		out = append(out, m)
	}
	return out
}

// byDesc decodes every metric matching desc (identity comparison — desc
// vars are package-level singletons, the same pattern promhttp itself
// uses to tell series apart), keyed by a chosen label's value.
func byDesc(t *testing.T, metrics []prometheus.Metric, desc *prometheus.Desc, keyLabel string) map[string]*dto.Metric {
	t.Helper()
	out := map[string]*dto.Metric{}
	for _, m := range metrics {
		if m.Desc() != desc {
			continue
		}
		var pm dto.Metric
		if err := m.Write(&pm); err != nil {
			t.Fatalf("Write: %v", err)
		}
		key := ""
		for _, l := range pm.GetLabel() {
			if l.GetName() == keyLabel {
				key = l.GetValue()
			}
		}
		out[key] = &pm
	}
	return out
}

func TestCollectNodeHealth(t *testing.T) {
	f := &fakeNode{health: &pb.HealthReport{
		Overall: pb.HealthReport_OVERALL_DEGRADED,
		Checks: []*pb.CheckResult{
			{Name: "disk", Status: pb.Health_HEALTH_HEALTHY},
			{Name: "mem", Status: pb.Health_HEALTH_DEGRADED},
		},
	}}
	c := NewCollector(f, nil)
	byCheck := byDesc(t, collect(t, c), nodeHealthDesc, "check")

	if got := byCheck["disk"].GetGauge().GetValue(); got != float64(pb.Health_HEALTH_HEALTHY) {
		t.Errorf("disk check = %v, want %v", got, pb.Health_HEALTH_HEALTHY)
	}
	if got := byCheck["mem"].GetGauge().GetValue(); got != float64(pb.Health_HEALTH_DEGRADED) {
		t.Errorf("mem check = %v, want %v", got, pb.Health_HEALTH_DEGRADED)
	}
}

func TestCollectResourceHealth(t *testing.T) {
	f := &fakeNode{
		health: &pb.HealthReport{},
		resources: &pb.ListResourcesResponse{Resources: []*pb.Resource{
			{Id: "default/web", Type: "file", Health: pb.Health_HEALTH_UNHEALTHY},
		}},
	}
	c := NewCollector(f, nil)
	byID := byDesc(t, collect(t, c), resourceHealthDesc, "id")

	got, ok := byID["default/web"]
	if !ok {
		t.Fatal("no metric for resource default/web")
	}
	if got.GetGauge().GetValue() != float64(pb.Health_HEALTH_UNHEALTHY) {
		t.Errorf("resource health = %v, want %v", got.GetGauge().GetValue(), pb.Health_HEALTH_UNHEALTHY)
	}
	if status := labelVal(got, "status"); status != "HEALTH_UNHEALTHY" {
		t.Errorf("status label = %q, want HEALTH_UNHEALTHY", status)
	}
}

func labelVal(m *dto.Metric, name string) string {
	for _, l := range m.GetLabel() {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}

func TestCollectQuorum(t *testing.T) {
	f := &fakeNode{
		health: &pb.HealthReport{},
		cluster: &control.Report{
			Leader: "n1:7444", QuorumHave: 3, QuorumNeed: 2, Degraded: false,
			Nodes: []control.NodeStatus{{ID: "n2", Degraded: true}},
		},
	}
	c := NewCollector(f, nil)
	metrics := collect(t, c)

	voters := byDesc(t, metrics, quorumVotersDesc, "")[""]
	if voters.GetGauge().GetValue() != 3 {
		t.Errorf("quorum voters = %v, want 3", voters.GetGauge().GetValue())
	}
	hasLeader := byDesc(t, metrics, quorumHasLeaderDesc, "")[""]
	if hasLeader.GetGauge().GetValue() != 1 {
		t.Errorf("has_leader = %v, want 1", hasLeader.GetGauge().GetValue())
	}
	n2 := byDesc(t, metrics, quorumNodeDegradedDesc, "node")["n2"]
	if n2.GetGauge().GetValue() != 1 {
		t.Errorf("n2's degraded gauge = %v, want 1 (self-reported degraded)", n2.GetGauge().GetValue())
	}
}

func TestCollectSkipsOnRPCError(t *testing.T) {
	f := &fakeNode{err: context.DeadlineExceeded}
	c := NewCollector(f, nil)
	if metrics := collect(t, c); len(metrics) != 0 {
		t.Errorf("expected no metrics when every RPC errors, got %d", len(metrics))
	}
}

func TestVolumeStateOrdinalMatchesProto(t *testing.T) {
	cases := map[storage.VolumeState]int32{
		storage.StateHealthy:             2,
		storage.StateDegraded:            3,
		storage.StateNeedsManualRecovery: 8,
	}
	for state, want := range cases {
		if got := volumeStateOrdinal(state); got != want {
			t.Errorf("volumeStateOrdinal(%s) = %d, want %d", state, got, want)
		}
	}
}
