package service

// Explain tests (T18, spec §7). Structure only — every node listed,
// specific per-node rejection reasons, score numbers for passing nodes.
// Human readability is the §10 exit-criteria item for manual review by
// a reviewer who did not write the scheduler (flagged in the commit).

import (
	"context"
	"strings"
	"testing"

	"github.com/expanse/expanse/internal/quantity"
	"github.com/expanse/expanse/internal/scheduler"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
)

// i32 is a replica-count helper.
func i32(v int32) *int32 { return &v }

// schedCfg mirrors the controller test's overcommit config.
func schedCfg() scheduler.OvercommitConfig {
	return scheduler.OvercommitConfig{CPUOvercommitRatio: 2.0, MemoryOvercommitRatio: 1.0}
}

// expNodes builds a 3-node cluster view: two healthy, one cordoned.
func expNodes() []scheduler.NodeView {
	good := scheduler.NodeView{
		ID:      "n1",
		Ready:   true,
		FreeCPU: quantity.CPU{Milli: 4000},
		FreeMem: quantity.Bytes{N: 8 << 30},
		FreeDisk: quantity.Bytes{
			N: 100 << 30,
		},
	}
	n2 := good
	n2.ID = "n2"
	n3 := good
	n3.ID = "n3"
	n3.Ready = false
	n3.Cordoned = true
	return []scheduler.NodeView{good, n2, n3}
}

// explainFixture creates a 3-replica anti-affinity block with replica 0
// already placed on n1 and returns the server plus the block name.
func explainFixture(t *testing.T) (*Server, string) {
	t.Helper()
	srv, _ := newServer(t)
	srv.Nodes = func(context.Context) ([]scheduler.NodeView, scheduler.OvercommitConfig, error) {
		return expNodes(), schedCfg(), nil
	}
	b := validBlock("web")
	b.Spec.Replicas = i32(3)
	b.Spec.Placement = &pb.Placement{AntiAffinity: pb.AntiAffinity_ANTI_AFFINITY_NODE}
	if _, err := srv.Create(context.Background(), b); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Simulate the controller having placed replica 0 on n1.
	status := &pb.BlockStatus{
		Phase: pb.Phase_PENDING,
		Placements: []*pb.PlacementStatus{{
			ReplicaIndex: 0,
			NodeId:       "n1",
			Phase:        pb.Phase_RUNNING,
			Generation:   1,
		}},
	}
	out, err := proto.Marshal(status)
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	if _, err := srv.St.Txn(context.Background(), []store.Op{
		{Kind: store.OpPut, Key: statusKeyRaw("default", "web"), Value: out},
	}); err != nil {
		t.Fatalf("put status: %v", err)
	}
	return srv, "web"
}

// Every node appears for each unplaced replica with its specific reason;
// passing nodes carry score numbers.
func TestExplainPerNodeReasonsAndScores(t *testing.T) {
	srv, name := explainFixture(t)
	resp, err := srv.Explain(context.Background(), &pb.ExplainRequest{Name: name})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if got := len(resp.GetReplicas()); got != 3 {
		t.Fatalf("replica explanations = %d, want 3", got)
	}
	placed := resp.GetReplicas()[0]
	if placed.GetNode() != "n1" || len(placed.GetNodes()) != 0 {
		t.Errorf("placed replica explanation = %+v, want node n1, no node table", placed)
	}
	for _, idx := range []int{1, 2} {
		re := resp.GetReplicas()[idx]
		if re.GetReplicaIndex() != int32(idx) {
			t.Fatalf("replica at slot %d = index %d", idx, re.GetReplicaIndex())
		}
		seen := map[string]*pb.NodeExplanation{}
		for _, n := range re.GetNodes() {
			seen[n.GetNode()] = n
		}
		if len(seen) != 3 {
			t.Fatalf("replica %d lists %d nodes, want all 3", idx, len(seen))
		}
		if n := seen["n1"]; n.GetOk() || !strings.Contains(n.GetReason(), "AntiAffinity") {
			t.Errorf("n1 = ok=%t reason=%q, want anti-affinity rejection", n.GetOk(), n.GetReason())
		}
		if n := seen["n3"]; n.GetOk() || !strings.Contains(n.GetReason(), "cordoned") {
			t.Errorf("n3 = ok=%t reason=%q, want specific cordoned rejection", n.GetOk(), n.GetReason())
		}
		if n := seen["n2"]; !n.GetOk() || n.GetScore() <= 0 || !strings.Contains(n.GetScoreDetail(), "S1") {
			t.Errorf("n2 = ok=%t score=%v detail=%q, want ok with S1..S6 breakdown", n.GetOk(), n.GetScore(), n.GetScoreDetail())
		}
		if re.GetSuggestion() != "" {
			t.Errorf("mixed rejections must not produce a suggestion, got %q", re.GetSuggestion())
		}
	}
}

// Uniform rejection across all nodes yields the one-line suggestion.
func TestExplainUniformRejectionSuggestion(t *testing.T) {
	srv, _ := newServer(t)
	srv.Nodes = func(context.Context) ([]scheduler.NodeView, scheduler.OvercommitConfig, error) {
		nodes := expNodes()
		for i := range nodes {
			nodes[i].Ready = false
			nodes[i].Cordoned = true
		}
		return nodes, schedCfg(), nil
	}
	b := validBlock("web")
	b.Spec.Replicas = i32(2)
	if _, err := srv.Create(context.Background(), b); err != nil {
		t.Fatalf("Create: %v", err)
	}
	resp, err := srv.Explain(context.Background(), &pb.ExplainRequest{Name: "web"})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	re := resp.GetReplicas()[0]
	for _, n := range re.GetNodes() {
		if n.GetOk() || !strings.Contains(n.GetReason(), "cordoned") {
			t.Errorf("node %s = %+v, want specific cordoned rejection", n.GetNode(), n)
		}
	}
	if !strings.Contains(re.GetSuggestion(), "uncordon") {
		t.Errorf("suggestion = %q, want uncordon hint", re.GetSuggestion())
	}
	// The rendered table shows the suggestion line.
	out := RenderExplain(resp)
	if !strings.Contains(out, "Suggestion: uncordon") {
		t.Errorf("rendered output missing suggestion:\n%s", out)
	}
}

// Explain 404s for unknown blocks and rejects empty names.
func TestExplainErrors(t *testing.T) {
	srv, _ := newServer(t)
	srv.Nodes = func(context.Context) ([]scheduler.NodeView, scheduler.OvercommitConfig, error) {
		return expNodes(), schedCfg(), nil
	}
	if _, err := srv.Explain(context.Background(), &pb.ExplainRequest{Name: "nope"}); err == nil {
		t.Error("expected NotFound for unknown block")
	}
	if _, err := srv.Explain(context.Background(), &pb.ExplainRequest{}); err == nil {
		t.Error("expected Invalid for empty name")
	}
}

// Rendered output contains the header, node rows, and score details.
func TestRenderExplainShape(t *testing.T) {
	srv, name := explainFixture(t)
	resp, err := srv.Explain(context.Background(), &pb.ExplainRequest{Name: name})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	out := RenderExplain(resp)
	for _, want := range []string{
		"Block default/web:",
		"Replica 0: placed on n1",
		"Replica 1: Pending",
		"n1", "n2", "n3",
		"✗", "✓",
		"AntiAffinity", "cordoned",
		"S1 least-loaded",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered output missing %q:\n%s", want, out)
		}
	}
}
