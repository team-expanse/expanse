// Stage 2 scoring (§4.1): S1–S6, each 0–100, weighted sum, highest wins.
// Ties break by a stable hash of blockID+replicaIndex+nodeID — no rand, no
// map iteration, no wall clock (§4.1 determinism requirement).

package scheduler

import (
	"fmt"
	"hash/fnv"
	"sort"
	"strings"

	pb "github.com/expanse/expanse/proto"
)

// Scoring weights (§4.1). Exact values are normative.
const (
	wLeastLoaded    = 10 // S1
	wSpreadTopology = 8  // S2
	wDataLocality   = 15 // S3
	wImageLocality  = 3  // S4
	wDeviceFit      = 5  // S5
	wStability      = 4  // S6

	maxWeightedScore = (wLeastLoaded + wSpreadTopology + wDataLocality +
		wImageLocality + wDeviceFit + wStability) * 100
)

// ClusterView carries the topology information S2 needs: how many replicas
// of the same block (same namespace, matching labels) already sit on each
// node.
type ClusterView struct {
	// SameBlockReplicas maps nodeID -> replica count of the block being
	// scheduled (any replica index).
	SameBlockReplicas map[string]int
}

// ScoredNode is one candidate with its weighted score.
type ScoredNode struct {
	NodeID string
	// Score is the weighted sum of S1–S6 (0..4500).
	Score int
	// Normalized is Score on a 0–100 scale.
	Normalized float64
}

// Score ranks candidates by S1–S6. The returned slice is sorted best-first:
// by Score descending, ties broken by the stable FNV-1a hash of
// blockID+replicaIndex+nodeID ascending (fully deterministic total order).
func Score(candidates []NodeView, req ReplicaRequest, cluster ClusterView) []ScoredNode {
	blockID := blockKey(req)
	blockVolumes := blockVolumeNames(req)
	deviceReqs := requestedDevices(req)

	maxFailures := 0
	for i := range candidates {
		if candidates[i].RecentFailures > maxFailures {
			maxFailures = candidates[i].RecentFailures
		}
	}
	scored := make([]ScoredNode, 0, len(candidates))
	for i := range candidates {
		n := candidates[i]
		total := wLeastLoaded*s1LeastLoaded(n) +
			wSpreadTopology*s2SpreadTopology(n.ID, cluster) +
			wDataLocality*s3DataLocality(n, blockVolumes) +
			wImageLocality*s4ImageLocality(n, req.Block.GetSpec().GetType()) +
			wDeviceFit*s5DeviceFit(n, deviceReqs) +
			wStability*s6Stability(n, maxFailures)
		scored = append(scored, ScoredNode{
			NodeID:     n.ID,
			Score:      total,
			Normalized: float64(total) / float64(maxWeightedScore) * 100,
		})
	}
	tie := make(map[string]uint64, len(scored))
	for i := range scored {
		tie[scored[i].NodeID] = tieBreakHash(blockID, req.ReplicaIndex, scored[i].NodeID)
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].Score != scored[j].Score {
			return scored[i].Score > scored[j].Score
		}
		return tie[scored[i].NodeID] < tie[scored[j].NodeID]
	})
	return scored
}

// Schedule combines stage 1 (Filter) and stage 2 (Score) for one replica.
// Returns the winning node ID, or a Pending reason built from every
// rejection.
func Schedule(nodes []NodeView, req ReplicaRequest, cfg OvercommitConfig, cluster ClusterView) (nodeID string, pending *pb.PendingReason) {
	cands, reasons := Filter(nodes, req, cfg)
	if len(cands) == 0 {
		return "", BuildPendingReason(reasons)
	}
	scored := Score(cands, req, cluster)
	return scored[0].NodeID, nil
}

func blockKey(req ReplicaRequest) string {
	md := req.Block.GetMetadata()
	return md.GetNamespace() + "/" + md.GetName()
}

func blockVolumeNames(req ReplicaRequest) []string {
	var out []string
	for _, st := range req.Block.GetSpec().GetStorage() {
		out = append(out, st.GetName())
	}
	return out
}

func requestedDevices(req ReplicaRequest) []*pb.Device {
	res := req.Resources
	if res == nil {
		res = req.Block.GetSpec().GetResources()
	}
	return res.GetDevices()
}

// S1 LeastLoaded: 100*(1 - utilization), utilization from CPU
// (free/capacity). Unknown capacity (0) is neutral: 0 points.
func s1LeastLoaded(n NodeView) int {
	if n.CapacityCPU.Milli <= 0 {
		return 0
	}
	util := 1 - float64(n.FreeCPU.Milli)/float64(n.CapacityCPU.Milli)
	if util < 0 {
		util = 0
	}
	return int(100 * (1 - util))
}

// S2 SpreadTopology: fewer replicas of the same block nearby. All candidates
// at the minimum count score 100; others scale down proportionally.
func s2SpreadTopology(nodeID string, cluster ClusterView) int {
	if len(cluster.SameBlockReplicas) == 0 {
		return 100
	}
	max := 0
	for _, c := range cluster.SameBlockReplicas {
		if c > max {
			max = c
		}
	}
	if max == 0 {
		return 100
	}
	return 100 * (max - cluster.SameBlockReplicas[nodeID]) / max
}

// S3 DataLocality: 100 if the node already holds a replica of one of the
// block's volumes.
func s3DataLocality(n NodeView, blockVolumes []string) int {
	for _, v := range blockVolumes {
		for _, h := range n.Volumes {
			if h == v {
				return 100
			}
		}
	}
	return 0
}

// S4 ImageLocality: 100 if the block type's nix closure is already realized.
func s4ImageLocality(n NodeView, blockType string) int {
	for _, t := range n.RealizedTypes {
		if t == blockType {
			return 100
		}
	}
	return 0
}

// S5 DeviceFit: prefer a tight fit so a big device is not stranded by a tiny
// request. Tightness = requested/free, capped at 100. No device request → 0.
func s5DeviceFit(n NodeView, reqs []*pb.Device) int {
	if len(reqs) == 0 {
		return 0
	}
	tight := 100
	for _, d := range reqs {
		free := n.Devices[d.GetType()]
		if free <= 0 {
			return 0 // cannot happen post-Filter; neutral
		}
		t := int(100 * int(d.GetCount()) / int(free))
		if t < tight {
			tight = t
		}
	}
	return tight
}

// S6 Stability: fewer recent failures is better. All candidates at zero (or
// at the max) score 100; others scale down proportionally.
func s6Stability(n NodeView, maxFailures int) int {
	if maxFailures <= 0 || n.RecentFailures >= maxFailures {
		return 100
	}
	return 100 * (maxFailures - n.RecentFailures) / maxFailures
}

// tieBreakHash: FNV-1a 64 over "blockID#replicaIndex#nodeID", truncated to
// 32 bits. Purely a function of the inputs — deterministic.
func tieBreakHash(blockID string, replicaIndex int, nodeID string) uint64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%s#%d#%s", blockID, replicaIndex, nodeID)
	return h.Sum64() & 0xffffffff
}

// ScoreBreakdown returns, per candidate node, the human-readable S1–S6
// contributions ("S1 least-loaded=78 (+390), ...") used by the explain
// table. Same inputs and arithmetic as Score; the max-failure anchor is
// computed across the candidate set so breakdown numbers match Score.
func ScoreBreakdown(candidates []NodeView, req ReplicaRequest, cluster ClusterView) map[string]string {
	blockVolumes := blockVolumeNames(req)
	deviceReqs := requestedDevices(req)
	maxFailures := 0
	for i := range candidates {
		if candidates[i].RecentFailures > maxFailures {
			maxFailures = candidates[i].RecentFailures
		}
	}
	out := make(map[string]string, len(candidates))
	for i := range candidates {
		n := candidates[i]
		parts := []string{
			fmt.Sprintf("S1 least-loaded=%d (+%d)", s1LeastLoaded(n), wLeastLoaded*s1LeastLoaded(n)),
			fmt.Sprintf("S2 spread=%d (+%d)", s2SpreadTopology(n.ID, cluster), wSpreadTopology*s2SpreadTopology(n.ID, cluster)),
			fmt.Sprintf("S3 locality=%d (+%d)", s3DataLocality(n, blockVolumes), wDataLocality*s3DataLocality(n, blockVolumes)),
			fmt.Sprintf("S4 image=%d (+%d)", s4ImageLocality(n, req.Block.GetSpec().GetType()), wImageLocality*s4ImageLocality(n, req.Block.GetSpec().GetType())),
			fmt.Sprintf("S5 device=%d (+%d)", s5DeviceFit(n, deviceReqs), wDeviceFit*s5DeviceFit(n, deviceReqs)),
			fmt.Sprintf("S6 stability=%d (+%d)", s6Stability(n, maxFailures), wStability*s6Stability(n, maxFailures)),
		}
		out[n.ID] = strings.Join(parts, ", ")
	}
	return out
}
