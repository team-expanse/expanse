package scheduler

import (
	"fmt"
	"strings"
	"testing"

	expstorage "github.com/expanse/expanse/internal/storage"
	pb "github.com/expanse/expanse/proto"
)

func baseNode(id string) NodeView {
	return NodeView{
		ID:           id,
		Ready:        true,
		FreeCPU:      cpuOf("4"),
		FreeMem:      bytesOf("8Gi"),
		FreeDisk:     bytesOf("100Gi"),
		Capabilities: []string{"kvm", "gpu"},
		Labels:       map[string]string{"zone": "a"},
		Devices:      map[string]int32{"gpu": 4},
		Arch:         "x86_64",
	}
}

func cpuOf(s string) quantityCPU { return mustCPU(s) }

func mustCPU(s string) quantityCPU {
	c, err := parseCPU(s)
	if err != nil {
		panic(err)
	}
	return c
}

func bytesOf(s string) quantityBytes {
	b, err := parseBytes(s)
	if err != nil {
		panic(err)
	}
	return b
}

func baseReq() ReplicaRequest {
	i := int32(2)
	return ReplicaRequest{
		Block: &pb.Block{
			Metadata: &pb.Metadata{Name: "web", Namespace: "default"},
			Spec: &pb.BlockSpec{
				Type:     "util/echo",
				Replicas: &i,
				Resources: &pb.Resources{Requests: &pb.ResourcePair{
					Cpu: "1", Memory: "1Gi",
				}},
			},
		},
		ReplicaIndex: 0,
	}
}

func baseCfg() OvercommitConfig {
	return OvercommitConfig{CPUOvercommitRatio: 1.0, MemoryOvercommitRatio: 1.0}
}

func single(t *testing.T, n NodeView, req ReplicaRequest) (accepted bool, reason string) {
	t.Helper()
	cands, reasons := Filter([]NodeView{n}, req, baseCfg())
	return len(cands) == 1, reasons[n.ID]
}

// Each predicate test: one node that fails ONLY that predicate, message
// assertion per §4.1.

func TestP1ReadyAndCordoned(t *testing.T) {
	req := baseReq()
	n := baseNode("n1")
	n.Ready = false
	if ok, r := single(t, n, req); ok || !strings.Contains(r, CodeNotReady) {
		t.Errorf("not-ready node accepted; reason=%q", r)
	}
	n = baseNode("n1")
	n.Cordoned = true
	if ok, r := single(t, n, req); ok || !strings.Contains(r, CodeNotReady) {
		t.Errorf("cordoned node accepted; reason=%q", r)
	}
}

func TestP2NotWitness(t *testing.T) {
	n := baseNode("n1")
	n.Witness = true
	if ok, r := single(t, n, baseReq()); ok || !strings.Contains(r, CodeWitness) {
		t.Errorf("witness accepted; reason=%q", r)
	}
}

func TestP3CPUFits(t *testing.T) {
	req := baseReq()
	n := baseNode("n1")
	n.FreeCPU = mustCPU("500m")
	if ok, r := single(t, n, req); ok || !strings.Contains(r, CodeInsufficientCPU) {
		t.Errorf("cpu-oversubscribed node accepted; reason=%q", r)
	}
	// Overcommit ratio 2.0 lets a 500m-free node fit a 1-core request.
	cfg := baseCfg()
	cfg.CPUOvercommitRatio = 2.0
	cands, _ := Filter([]NodeView{n}, req, cfg)
	if len(cands) != 1 {
		t.Errorf("overcommit ratio not applied: %v", cands)
	}
}

func TestP4MemoryFits(t *testing.T) {
	req := baseReq()
	n := baseNode("n1")
	n.FreeMem = bytesOf("512Mi")
	if ok, r := single(t, n, req); ok || !strings.Contains(r, CodeInsufficientMemory) {
		t.Errorf("memory-oversubscribed node accepted; reason=%q", r)
	}
}

func TestP5DiskFits(t *testing.T) {
	req := baseReq()
	i := int32(1)
	req.Block.Spec.Replicas = &i
	req.Block.Spec.Storage = []*pb.Storage{{Name: "data", Size: "50Gi"}}
	n := baseNode("n1")
	n.FreeDisk = bytesOf("10Gi")
	if ok, r := single(t, n, req); ok || !strings.Contains(r, CodeInsufficientDisk) {
		t.Errorf("disk-starved node accepted; reason=%q", r)
	}
	// Enough disk passes.
	n.FreeDisk = bytesOf("60Gi")
	if ok, _ := single(t, n, req); !ok {
		t.Error("disk-fit node rejected")
	}
}

func TestP6RequiredCapabilities(t *testing.T) {
	req := baseReq()
	req.Placement = &pb.Placement{RequiredCapabilities: []string{"gpu", "tpu"}}
	n := baseNode("n1") // has kvm,gpu — missing tpu
	if ok, r := single(t, n, req); ok || !strings.Contains(r, CodeMissingCapability) || !strings.Contains(r, "tpu") {
		t.Errorf("capability-missing node accepted; reason=%q", r)
	}
	n.Capabilities = append(n.Capabilities, "tpu")
	if ok, _ := single(t, n, req); !ok {
		t.Error("capability-fit node rejected")
	}
}

func TestP7NodeSelector(t *testing.T) {
	req := baseReq()
	req.Placement = &pb.Placement{NodeSelector: map[string]string{"zone": "b"}}
	n := baseNode("n1") // zone=a
	if ok, r := single(t, n, req); ok || !strings.Contains(r, CodeNodeSelectorMatch) {
		t.Errorf("selector-mismatched node accepted; reason=%q", r)
	}
	n.Labels["zone"] = "b"
	if ok, _ := single(t, n, req); !ok {
		t.Error("selector-matching node rejected")
	}
}

func TestP8AntiAffinity(t *testing.T) {
	req := baseReq()
	req.Placement = &pb.Placement{AntiAffinity: pb.AntiAffinity_ANTI_AFFINITY_NODE}
	req.ExistingPlacements = []string{"n1"}
	n := baseNode("n1")
	if ok, r := single(t, n, req); ok || !strings.Contains(r, CodeAntiAffinity) {
		t.Errorf("anti-affinity conflict accepted; reason=%q", r)
	}
	// Without anti-affinity the same node passes.
	req.Placement = &pb.Placement{}
	if ok, _ := single(t, n, req); !ok {
		t.Error("no-anti-affinity node rejected")
	}
}

func TestP9Devices(t *testing.T) {
	req := baseReq()
	req.Resources = &pb.Resources{
		Requests: &pb.ResourcePair{Cpu: "1", Memory: "1Gi"},
		Devices:  []*pb.Device{{Type: "gpu", Count: 2}},
	}
	n := baseNode("n1") // 4 free gpus
	if ok, _ := single(t, n, req); !ok {
		t.Error("device-fit node rejected")
	}
	n.Devices = map[string]int32{"gpu": 1}
	if ok, r := single(t, n, req); ok || !strings.Contains(r, CodeDeviceUnavailable) || !strings.Contains(r, "gpu") {
		t.Errorf("device-short node accepted; reason=%q", r)
	}
	n.Devices = nil
	if ok, r := single(t, n, req); ok || !strings.Contains(r, CodeDeviceUnavailable) {
		t.Errorf("device-less node accepted; reason=%q", r)
	}
}

func TestP10Taints(t *testing.T) {
	req := baseReq()
	req.Placement = &pb.Placement{Tolerations: []string{"storage"}}
	n := baseNode("n1")
	n.Taints = []string{"storage", "gpu-only"}
	if ok, r := single(t, n, req); ok || !strings.Contains(r, CodeTaintNotTolerated) || !strings.Contains(r, "gpu-only") {
		t.Errorf("tainted node accepted; reason=%q", r)
	}
	req.Placement.Tolerations = []string{"storage", "gpu-only"}
	if ok, _ := single(t, n, req); !ok {
		t.Error("taint-tolerating node rejected")
	}
}

func TestP11Arch(t *testing.T) {
	req := baseReq()
	req.Arches = []string{"aarch64"}
	n := baseNode("n1") // x86_64
	if ok, r := single(t, n, req); ok || !strings.Contains(r, CodeArchMismatch) {
		t.Errorf("arch-mismatched node accepted; reason=%q", r)
	}
	req.Arches = []string{"aarch64", "x86_64"}
	if ok, _ := single(t, n, req); !ok {
		t.Error("arch-matching node rejected")
	}
	// Empty Arches means any.
	req.Arches = nil
	if ok, _ := single(t, n, req); !ok {
		t.Error("any-arch node rejected")
	}
}

// TestP12VolumeColocation is the regression test for PHASE-03-TASKS.md D2: a
// SINGLETON block bound to a volume may only place on a node that already
// holds a healthy replica of it.
func TestP12VolumeColocation(t *testing.T) {
	req := baseReq()
	req.Block.Spec.Strategy = &pb.Strategy{Kind: pb.StrategyKind_SINGLETON}
	req.Block.Spec.Storage = []*pb.Storage{{Name: "share-data", Size: "10Gi"}}
	// baseReq's block is default/web; matched by its auto-provisioned
	// composite name (expstorage.BlockVolumeName), not the storage
	// entry's own raw "share-data" name.
	vname := expstorage.BlockVolumeName("default", "web", "share-data")
	n := baseNode("n1")
	n.FreeDisk = bytesOf("100Gi")

	// No local replica at all: rejected.
	if ok, r := single(t, n, req); ok || !strings.Contains(r, CodeVolumeNotLocal) || !strings.Contains(r, "share-data") {
		t.Errorf("node with no volume replica accepted; reason=%q", r)
	}
	// A replica exists but is not healthy (e.g. resyncing): still rejected.
	n.Volumes = []string{vname}
	if ok, r := single(t, n, req); ok || !strings.Contains(r, CodeVolumeNotLocal) {
		t.Errorf("node with only an unhealthy replica accepted; reason=%q", r)
	}
	// A healthy replica: accepted.
	n.HealthyVolumes = []string{vname}
	if ok, _ := single(t, n, req); !ok {
		t.Error("node with a healthy replica of the bound volume rejected")
	}
	// Non-SINGLETON strategies are unaffected by P12 even with no replica.
	req.Block.Spec.Strategy = &pb.Strategy{Kind: pb.StrategyKind_PRIMARY_REPLICA}
	n.HealthyVolumes = nil
	if ok, r := single(t, n, req); !ok {
		t.Errorf("non-SINGLETON block wrongly gated by P12; reason=%q", r)
	}
}

// TestD42MemoryNeverOvercommitted is the regression test for decision D4.2:
// even with memoryOvercommitRatio 2.0 passed in, a node with 512Mi free must
// not fit a 1Gi request.
func TestD42MemoryNeverOvercommitted(t *testing.T) {
	req := baseReq()
	n := baseNode("n1")
	n.FreeMem = bytesOf("512Mi")
	cfg := baseCfg()
	cfg.MemoryOvercommitRatio = 2.0
	cands, reasons := Filter([]NodeView{n}, req, cfg)
	if len(cands) != 0 {
		t.Fatal("memory was overcommitted despite D4.2")
	}
	if !strings.Contains(reasons["n1"], CodeInsufficientMemory) {
		t.Errorf("unexpected reason %q", reasons["n1"])
	}
	// And the clamp is visible in availMem directly.
	if got := availMem(n, cfg); got.N != n.FreeMem.N {
		t.Errorf("availMem = %s, want unclamped %s", got, n.FreeMem)
	}
}

func TestReservedResources(t *testing.T) {
	req := baseReq() // 1 cpu, 1Gi
	n := baseNode("n1")
	n.FreeCPU = mustCPU("1200m")
	n.FreeMem = bytesOf("1200Mi")
	cfg := baseCfg()
	cfg.ReservedCPU = mustCPU("500m")
	cfg.ReservedMemory = bytesOf("512Mi")
	// 1200-500=700m < 1000m; 1200-512=688Mi < 1Gi.
	cands, reasons := Filter([]NodeView{n}, req, cfg)
	if len(cands) != 0 {
		t.Fatal("reserved resources not subtracted")
	}
	for _, code := range []string{CodeInsufficientCPU, CodeInsufficientMemory} {
		if !strings.Contains(reasons["n1"], code) && !strings.Contains(reasons["n1"], "available") {
			t.Errorf("reason %q missing for %s", reasons["n1"], code)
		}
	}
	n.FreeCPU = mustCPU("2")
	n.FreeMem = bytesOf("2Gi")
	cands, _ = Filter([]NodeView{n}, req, cfg)
	if len(cands) != 1 {
		t.Error("node with headroom above reserve rejected")
	}
}

// TestFilterIntegration runs the combined filter on a small mixed cluster.
func TestFilterIntegration(t *testing.T) {
	req := baseReq()
	req.Placement = &pb.Placement{
		AntiAffinity:         pb.AntiAffinity_ANTI_AFFINITY_NODE,
		RequiredCapabilities: []string{"gpu"},
		Tolerations:          []string{"storage"},
	}
	req.ExistingPlacements = []string{"n1"}
	nodes := []NodeView{
		func() NodeView { n := baseNode("n1"); return n }(),                                // anti-affinity conflict
		func() NodeView { n := baseNode("n2"); n.Witness = true; return n }(),              // witness
		func() NodeView { n := baseNode("n3"); n.Capabilities = nil; return n }(),          // missing gpu
		func() NodeView { n := baseNode("n4"); n.Taints = []string{"sealed"}; return n }(), // untolerated taint
		baseNode("n5"), // fits
	}
	cands, reasons := Filter(nodes, req, baseCfg())
	if len(cands) != 1 || cands[0].ID != "n5" {
		t.Fatalf("candidates = %v, want only n5", ids(cands))
	}
	wantCodes := map[string]string{
		"n1": CodeAntiAffinity, "n2": CodeWitness,
		"n3": CodeMissingCapability, "n4": CodeTaintNotTolerated,
	}
	for id, code := range wantCodes {
		if !strings.HasPrefix(reasons[id], code+":") {
			t.Errorf("n=%s reason %q, want prefix %q", id, reasons[id], code)
		}
	}

	pending := BuildPendingReason(map[string]string{
		"n1": reasons["n1"], "n2": reasons["n2"], "n3": reasons["n3"], "n4": reasons["n4"],
	})
	if pending == nil {
		t.Fatal("BuildPendingReason returned nil")
	}
	if pending.Code != CodeAntiAffinity {
		t.Errorf("unexpected code %q", pending.Code)
	}
	if len(pending.PerNode) != 4 {
		t.Errorf("PerNode = %v, want 4 entries", pending.PerNode)
	}
	if !strings.Contains(pending.Message, "anti-affinity") && !strings.Contains(pending.Message, CodeAntiAffinity) {
		t.Errorf("Message %q not human-meaningful", pending.Message)
	}
}

func ids(nodes []NodeView) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.ID
	}
	return out
}

// TestBuildPendingReasonMostCommon exercises the most-common-code election
// and the tie-break.
func TestBuildPendingReasonMostCommon(t *testing.T) {
	p := BuildPendingReason(map[string]string{
		"n1": CodeInsufficientMemory + ": needs 4Gi",
		"n2": CodeInsufficientMemory + ": needs 4Gi",
		"n3": CodeAntiAffinity + ": conflict",
	})
	if p == nil || p.Code != CodeInsufficientMemory {
		t.Fatalf("code = %+v, want InsufficientMemory", p)
	}
	if !strings.Contains(p.Message, "needs 4Gi") {
		t.Errorf("Message = %q", p.Message)
	}
	if p.PerNode["n3"] != "anti-affinity conflict" && p.PerNode["n3"] != "conflict" {
		t.Logf("PerNode[n3] = %q", p.PerNode["n3"])
	}

	// Tie between two codes breaks alphabetically.
	p = BuildPendingReason(map[string]string{
		"n1": CodeWitness + ": witness",
		"n2": CodeAntiAffinity + ": conflict",
	})
	if p.Code != CodeAntiAffinity {
		t.Errorf("tie-break code = %q, want AntiAffinityConflict", p.Code)
	}

	// Nil for empty input (nothing pending).
	if BuildPendingReason(nil) != nil {
		t.Error("empty reasons should produce nil")
	}
}

// TestFilterDeterminism: same input, same result — no map iteration order
// leaking into candidates or reasons.
func TestFilterDeterminism(t *testing.T) {
	req := baseReq()
	var nodes []NodeView
	for i := 1; i <= 30; i++ {
		n := baseNode(fmt.Sprintf("n%02d", i))
		if i%3 == 0 {
			n.Ready = false
		}
		if i%5 == 0 {
			n.FreeMem = bytesOf("512Mi")
		}
		nodes = append(nodes, n)
	}
	first := ""
	for run := 0; run < 100; run++ {
		cands, reasons := Filter(nodes, req, baseCfg())
		sig := fmt.Sprintf("%v|%v", ids(cands), reasons)
		if run == 0 {
			first = sig
			continue
		}
		if sig != first {
			t.Fatalf("run %d differs from run 0", run)
		}
	}
}
