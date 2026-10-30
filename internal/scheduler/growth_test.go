package scheduler

import (
	"strings"
	"testing"

	pb "github.com/expanse/expanse/proto"
)

func withRequests(cpu, mem string) ReplicaRequest {
	r := baseReq()
	r.Block.Spec.Resources = &pb.Resources{Requests: &pb.ResourcePair{Cpu: cpu, Memory: mem}}
	return r
}

func TestAGrowthWithinTheNodesFreeCapacityFits(t *testing.T) {
	n := baseNode("n1") // 4 cpu, 8Gi free beyond the running replica
	if r := GrowthMisfit(n, withRequests("1", "1Gi"), withRequests("4", "9Gi"), baseCfg()); r != "" {
		t.Errorf("growth by 3 cpu and 8Gi refused on a node with 4 cpu and 8Gi free: %q", r)
	}
}

func TestAGrowthPastTheNodesFreeMemoryIsRefused(t *testing.T) {
	n := baseNode("n1")
	r := GrowthMisfit(n, withRequests("1", "1Gi"), withRequests("1", "10Gi"), baseCfg())
	if !strings.HasPrefix(r, CodeInsufficientMemory) || !strings.Contains(r, "n1") {
		t.Errorf("growth by 9Gi on a node with 8Gi free: reason %q", r)
	}
}

func TestAGrowthPastTheNodesFreeCPUIsRefused(t *testing.T) {
	n := baseNode("n1")
	r := GrowthMisfit(n, withRequests("1", "1Gi"), withRequests("6", "1Gi"), baseCfg())
	if !strings.HasPrefix(r, CodeInsufficientCPU) {
		t.Errorf("growth by 5 cpu on a node with 4 free: reason %q", r)
	}
}

func TestAnUnchangedRequestOnAnOvercommittedNodeIsNotRefused(t *testing.T) {
	n := baseNode("n1")
	n.FreeMem.N = -1 // already past capacity from other blocks
	if r := GrowthMisfit(n, withRequests("1", "2Gi"), withRequests("2", "2Gi"), baseCfg()); r != "" {
		t.Errorf("memory did not grow, yet refused: %q", r)
	}
}

func TestAShrinkAlwaysFits(t *testing.T) {
	n := baseNode("n1")
	n.FreeCPU, n.FreeMem = cpuOf("0"), bytesOf("0")
	if r := GrowthMisfit(n, withRequests("2", "2Gi"), withRequests("1", "1Gi"), baseCfg()); r != "" {
		t.Errorf("shrink refused: %q", r)
	}
}
