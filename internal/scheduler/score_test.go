package scheduler

import (
	"fmt"
	"sort"
	"testing"
	"time"
)

func scoreNode() NodeView {
	n := baseNode("n1")
	n.CapacityCPU = mustCPU("4")
	return n
}

func TestScoreS1LeastLoaded(t *testing.T) {
	req := baseReq()
	loaded := scoreNode()
	loaded.ID = "busy"
	loaded.FreeCPU = mustCPU("500m") // utilization 87.5%
	idle := scoreNode()
	idle.ID = "idle"
	scored := Score([]NodeView{loaded, idle}, req, ClusterView{})
	if scored[0].NodeID != "idle" {
		t.Errorf("idle node not ranked first: %+v", scored)
	}
	if scored[1].Score >= scored[0].Score {
		t.Errorf("busy node scored >= idle: %+v", scored)
	}
}

func TestScoreS2SpreadTopology(t *testing.T) {
	req := baseReq()
	a := scoreNode() // 2 replicas of this block
	a.ID = "n1"
	a.FreeCPU = a.CapacityCPU
	b := scoreNode() // 0 replicas
	b.ID = "n2"
	b.FreeCPU = b.CapacityCPU
	scored := Score([]NodeView{a, b}, req, ClusterView{
		SameBlockReplicas: map[string]int{a.ID: 2},
	})
	if scored[0].NodeID != b.ID {
		t.Errorf("node with fewer co-located replicas not first: %+v", scored)
	}
	if scored[0].Score <= scored[1].Score {
		t.Errorf("spread did not differentiate: %+v", scored)
	}
}

func TestScoreS3DataLocality(t *testing.T) {
	req := baseReq()
	i := int32(1)
	req.Block.Spec.Replicas = &i
	req.Block.Spec.Storage = []*pbStorage{{Name: "data", Size: "1Gi"}}
	local := scoreNode()
	local.ID = "n1"
	local.Volumes = []string{"data"}
	remote := scoreNode()
	remote.ID = "n2"
	scored := Score([]NodeView{remote, local}, req, ClusterView{})
	if scored[0].NodeID != local.ID {
		t.Errorf("volume-local node not first: %+v", scored)
	}
}

func TestScoreS4ImageLocality(t *testing.T) {
	req := baseReq()
	warm := scoreNode()
	warm.ID = "n1"
	warm.RealizedTypes = []string{"util/echo"}
	cold := scoreNode()
	cold.ID = "n2"
	scored := Score([]NodeView{cold, warm}, req, ClusterView{})
	if scored[0].NodeID != warm.ID {
		t.Errorf("image-warm node not first: %+v", scored)
	}
}

func TestScoreS5DeviceFit(t *testing.T) {
	req := baseReq()
	req.Resources = &pbResources{
		Requests: &pbResourcePair{Cpu: "1", Memory: "1Gi"},
		Devices:  []*pbDevice{{Type: "gpu", Count: 2}},
	}
	tight := scoreNode() // 2 free gpus: perfect fit (100)
	tight.ID = "n1"
	tight.Devices = map[string]int32{"gpu": 2}
	stranded := scoreNode() // 8 free gpus: 2/8 = 25
	stranded.ID = "n2"
	stranded.Devices = map[string]int32{"gpu": 8}
	scored := Score([]NodeView{stranded, tight}, req, ClusterView{})
	if scored[0].NodeID != tight.ID {
		t.Errorf("tight-fit node not first: %+v", scored)
	}
	if scored[0].Score <= scored[1].Score {
		t.Errorf("device fit did not differentiate: %+v", scored)
	}
}

func TestScoreS6Stability(t *testing.T) {
	req := baseReq()
	steady := scoreNode()
	steady.ID = "n1"
	flaky := scoreNode()
	flaky.ID = "n2"
	flaky.RecentFailures = 5
	scored := Score([]NodeView{flaky, steady}, req, ClusterView{})
	if scored[0].NodeID != steady.ID {
		t.Errorf("stable node not first: %+v", scored)
	}
	// All-stable is neutral: equal scores.
	eq := Score([]NodeView{steady, scoreNode()}, req, ClusterView{})
	if eq[0].Score != eq[1].Score {
		t.Errorf("all-stable nodes scored differently: %+v", eq)
	}
}

func TestScoreWeightsSum(t *testing.T) {
	// Full marks everywhere must give exactly (10+8+15+3+5+4)*100 = 4500.
	req := baseReq()
	n := scoreNode()
	n.FreeCPU = n.CapacityCPU
	n.Volumes = []string{"data"}
	n.RealizedTypes = []string{"util/echo"}
	n.RecentFailures = 0
	req.Resources = &pbResources{
		Requests: &pbResourcePair{Cpu: "1", Memory: "1Gi"},
		Devices:  []*pbDevice{{Type: "gpu", Count: 1}},
	}
	n.Devices = map[string]int32{"gpu": 1} // perfect device fit
	req.Block.Spec.Storage = []*pbStorage{{Name: "data", Size: "1Gi"}}
	scored := Score([]NodeView{n}, req, ClusterView{SameBlockReplicas: map[string]int{n.ID: 0}})
	if scored[0].Score != 4500 {
		t.Errorf("full-marks score = %d, want 4500", scored[0].Score)
	}
	if scored[0].Normalized != 100 {
		t.Errorf("normalized = %v, want 100", scored[0].Normalized)
	}
}

// TestScoreTieBreakDeterministic: identical candidates must always be
// ordered by the stable hash — same order every run, no map iteration.
func TestScoreTieBreakDeterministic(t *testing.T) {
	req := baseReq()
	var nodes []NodeView
	for i := 1; i <= 8; i++ {
		n := scoreNode()
		n.ID = fmt.Sprintf("n%d", i)
		nodes = append(nodes, n)
	}
	var first []string
	for run := 0; run < 100; run++ {
		scored := Score(nodes, req, ClusterView{})
		ids := make([]string, len(scored))
		for i, s := range scored {
			ids[i] = s.NodeID
		}
		if run == 0 {
			first = ids
			// Ties mean equal scores.
			if scored[0].Score != scored[len(scored)-1].Score {
				t.Fatalf("nodes not tied: %+v", scored)
			}
			// Hash order ascending.
			for i := 1; i < len(ids); i++ {
				if tieBreakHash(blockKey(req), req.ReplicaIndex, ids[i-1]) >
					tieBreakHash(blockKey(req), req.ReplicaIndex, ids[i]) {
					t.Fatalf("order not hash-ascending: %v", ids)
				}
			}
			continue
		}
		if fmt.Sprint(ids) != fmt.Sprint(first) {
			t.Fatalf("run %d order differs", run)
		}
	}
}

// TestScheduleDeterminism is the spec test: schedule the same fixed input
// 1000 times; the result set has exactly one unique value.
func TestScheduleDeterminism(t *testing.T) {
	req := baseReq()
	nodes := make([]NodeView, 10)
	for i := range nodes {
		n := scoreNode()
		n.ID = fmt.Sprintf("n%d", i)
		nodes[i] = n
	}
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id, pending := Schedule(nodes, req, baseCfg(), ClusterView{})
		if pending != nil {
			t.Fatalf("unexpected pending: %v", pending)
		}
		seen[id] = true
	}
	if len(seen) != 1 {
		t.Fatalf("non-deterministic schedule: %v unique results", len(seen))
	}
}

// TestSchedulePending: no candidates -> pending with per-node breakdown.
func TestSchedulePending(t *testing.T) {
	req := baseReq()
	n := baseNode("n1")
	n.Ready = false
	id, pending := Schedule([]NodeView{n}, req, baseCfg(), ClusterView{})
	if id != "" || pending == nil {
		t.Fatalf("expected pending, got id=%q pending=%v", id, pending)
	}
	if pending.Code != CodeNotReady || len(pending.PerNode) != 1 {
		t.Errorf("pending = %+v", pending)
	}
}

// TestScheduleTimed gates G4.4: 100 blocks x 3 replicas across 10 nodes
// must complete within 500 ms wall clock (asserted in-test, not bench).
func TestScheduleTimed(t *testing.T) {
	nodes := make([]NodeView, 10)
	for i := range nodes {
		n := scoreNode()
		n.ID = fmt.Sprintf("n%d", i)
		nodes[i] = n
	}
	start := time.Now()
	for b := 0; b < 100; b++ {
		req := baseReq()
		i := int32(2)
		req.Block.Metadata.Name = fmt.Sprintf("block%02d", b)
		req.Block.Spec.Replicas = &i
		for r := 0; r < 3; r++ {
			req.ReplicaIndex = r
			// Simulate placements accumulating for anti-affinity realism.
			req.ExistingPlacements = []string{fmt.Sprintf("n%d", r)}
			id, pending := Schedule(nodes, req, baseCfg(), ClusterView{})
			if pending != nil {
				t.Fatalf("block %d replica %d pending: %v", b, r, pending)
			}
			if id == "" {
				t.Fatalf("empty node id")
			}
		}
	}
	elapsed := time.Since(start)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("300 placements took %v, budget 500ms", elapsed)
	}
	t.Logf("300 placements in %v", elapsed)
}

// TestScoreAllWeightsApplied sanity-checks that every S contributes: bumping
// only S3's input never lowers the score.
func TestScoreMonotoneS3(t *testing.T) {
	req := baseReq()
	i := int32(1)
	req.Block.Spec.Replicas = &i
	req.Block.Spec.Storage = []*pbStorage{{Name: "data", Size: "1Gi"}}
	a := scoreNode()
	b := scoreNode()
	b.Volumes = []string{"data"}
	sa := Score([]NodeView{a}, req, ClusterView{})
	sb := Score([]NodeView{b}, req, ClusterView{})
	if sb[0].Score != sa[0].Score+15*100 {
		t.Errorf("S3 weight wrong: %d vs %d", sb[0].Score, sa[0].Score)
	}
	sort.Slice(sa, func(i, j int) bool { return sa[i].NodeID < sa[j].NodeID })
}
