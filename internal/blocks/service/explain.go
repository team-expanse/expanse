package service

// Explain (Phase 04 T18, spec §7): "why is this pending?" — for every
// unplaced replica, re-run Filter+Score fresh against current cluster
// state and report every node's pass/fail predicate and, for nodes that
// passed, the S1–S6 score breakdown.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/scheduler"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
)

// statusKeyRaw builds the observed-state key for a block (mirror of the
// controller's statusKey; the controller owns placement, this package
// only reads).
func statusKeyRaw(ns, name string) store.Key {
	return store.Key("/blocks/" + ns + "/" + name + "/status")
}

// desiredReplicas resolves the replica count for explanation: singleton
// → 1, daemonset → number of eligible nodes (Ready, non-witness), else
// spec.replicas.
func desiredReplicas(b *pb.Block, nodes []scheduler.NodeView) int {
	switch b.GetSpec().GetStrategy().GetKind() {
	case pb.StrategyKind_SINGLETON:
		return 1
	case pb.StrategyKind_DAEMONSET:
		n := 0
		for _, v := range nodes {
			if v.Ready && !v.Witness {
				n++
			}
		}
		return n
	}
	return int(b.GetSpec().GetReplicas())
}

// suggestionFor maps a uniform rejection code to a one-line actionable
// hint (spec §7 example: "add a 4th node, or set placement.antiAffinity
// =none").
func suggestionFor(code string) string {
	switch code {
	case scheduler.CodeNotReady:
		return "uncordon the nodes, or join more nodes to the cluster"
	case scheduler.CodeWitness:
		return "witness nodes never host blocks; join a non-witness node"
	case scheduler.CodeInsufficientCPU:
		return "add node capacity, or lower spec.resources.requests.cpu"
	case scheduler.CodeInsufficientMemory:
		return "add node memory, or lower spec.resources.requests.memory (memory is never overcommitted)"
	case scheduler.CodeInsufficientDisk:
		return "add node disk, or shrink spec.storage sizes"
	case scheduler.CodeMissingCapability:
		return "join a node advertising the required capabilities"
	case scheduler.CodeNodeSelectorMatch:
		return "fix spec.placement.node_selector labels, or relabel nodes"
	case scheduler.CodeAntiAffinity:
		return "add more nodes, or set placement.anti_affinity=none"
	case scheduler.CodeDeviceUnavailable:
		return "add the required devices, or drop them from spec.resources.devices"
	case scheduler.CodeTaintNotTolerated:
		return "add a matching toleration, or remove the taint"
	case scheduler.CodeArchMismatch:
		return "join a node with the required architecture"
	}
	return ""
}

// Explain implements BlockService.Explain.
func (s *Server) Explain(ctx context.Context, r *pb.ExplainRequest) (*pb.ExplainResponse, error) {
	ns := r.GetNamespace()
	if ns == "" {
		ns = "default"
	}
	if r.GetName() == "" {
		return nil, errors.New(errors.KindInvalid, "block.Explain", "name is required")
	}
	if s.Nodes == nil {
		return nil, errors.New(errors.KindUnavailable, "block.Explain", "no cluster view configured")
	}
	nodes, cfg, err := s.Nodes(ctx)
	if err != nil {
		return nil, errors.Wrap(err, errors.KindUnavailable, "block.Explain", "cluster view")
	}

	e, err := s.St.Get(ctx, key(ns, r.GetName()))
	if err != nil {
		if errors.Is(err, errors.KindNotFound) {
			return nil, errors.New(errors.KindNotFound, "block.Explain", ns+"/"+r.GetName())
		}
		return nil, errors.Wrap(err, errors.KindInternal, "block.Explain", "get block")
	}
	var b pb.Block
	if err := proto.Unmarshal(e.Value, &b); err != nil {
		return nil, errors.Wrap(err, errors.KindInternal, "block.Explain", "unmarshal block")
	}

	resp := &pb.ExplainResponse{Namespace: ns, Name: r.GetName()}
	status, err := s.loadExplainStatus(ctx, ns, r.GetName())
	if err != nil {
		return nil, err
	}
	resp.Phase = status.GetPhase()
	resp.PendingReason = status.GetPendingReason()

	// Active placements: index-addressed records that still hold their
	// slot (retired records have index -1).
	active := map[int32]*pb.PlacementStatus{}
	existing := make([]string, 0, len(status.GetPlacements()))
	for _, p := range status.GetPlacements() {
		if p.GetReplicaIndex() >= 0 && p.GetPhase() != pb.Phase_LOST {
			active[p.GetReplicaIndex()] = p
			existing = append(existing, p.GetNodeId())
		}
	}

	want := desiredReplicas(&b, nodes)
	for i := 0; i < want; i++ {
		re := &pb.ReplicaExplanation{ReplicaIndex: int32(i)}
		if p := active[int32(i)]; p != nil {
			re.Phase = p.GetPhase()
			re.Node = p.GetNodeId()
			resp.Replicas = append(resp.Replicas, re)
			continue
		}
		// Unplaced: re-run Filter+Score fresh (§7) and explain every node.
		re.Phase = pb.Phase_PENDING
		candidates, reasons := scheduler.Filter(nodes, scheduler.ReplicaRequest{
			Block:              &b,
			ReplicaIndex:       i,
			ExistingPlacements: existing,
		}, cfg)
		scored := scheduler.Score(candidates, scheduler.ReplicaRequest{
			Block:              &b,
			ReplicaIndex:       i,
			ExistingPlacements: existing,
		}, scheduler.ClusterView{SameBlockReplicas: sameBlockCounts(existing)})
		breakdowns := scheduler.ScoreBreakdown(candidates, scheduler.ReplicaRequest{
			Block:              &b,
			ReplicaIndex:       i,
			ExistingPlacements: existing,
		}, scheduler.ClusterView{SameBlockReplicas: sameBlockCounts(existing)})
		scoreByNode := map[string]*scheduler.ScoredNode{}
		for j := range scored {
			scoreByNode[scored[j].NodeID] = &scored[j]
		}
		// Order: passing nodes best-first, then failing nodes by ID.
		passing := make([]string, 0, len(candidates))
		for _, n := range candidates {
			passing = append(passing, n.ID)
		}
		sort.Slice(passing, func(a, c int) bool {
			return scoreByNode[passing[a]].Score > scoreByNode[passing[c]].Score
		})
		failing := make([]string, 0, len(nodes)-len(candidates))
		for _, n := range nodes {
			if _, ok := reasons[n.ID]; ok {
				failing = append(failing, n.ID)
			}
		}
		sort.Strings(failing)
		for _, id := range passing {
			re.Nodes = append(re.Nodes, &pb.NodeExplanation{
				Node:        id,
				Ok:          true,
				Score:       scoreByNode[id].Normalized,
				ScoreDetail: breakdowns[id],
			})
		}
		for _, id := range failing {
			re.Nodes = append(re.Nodes, &pb.NodeExplanation{
				Node:   id,
				Ok:     false,
				Reason: reasons[id],
			})
		}
		// Uniform rejection → one-line suggestion.
		if len(reasons) == len(nodes) && len(nodes) > 0 {
			code, _, _ := strings.Cut(reasons[nodes[0].ID], ": ")
			uniform := true
			for _, rr := range reasons {
				c, _, _ := strings.Cut(rr, ": ")
				if c != code {
					uniform = false
					break
				}
			}
			if uniform {
				re.Suggestion = suggestionFor(code)
			}
		}
		resp.Replicas = append(resp.Replicas, re)
	}
	return resp, nil
}

// loadExplainStatus reads the block's observed-state record; a missing
// record means never-placed (Pending).
func (s *Server) loadExplainStatus(ctx context.Context, ns, name string) (*pb.BlockStatus, error) {
	e, err := s.St.Get(ctx, statusKeyRaw(ns, name))
	if err != nil {
		if errors.Is(err, errors.KindNotFound) {
			return &pb.BlockStatus{Phase: pb.Phase_PENDING}, nil
		}
		return nil, errors.Wrap(err, errors.KindInternal, "block.Explain", "get status")
	}
	var st pb.BlockStatus
	if err := proto.Unmarshal(e.Value, &st); err != nil {
		return nil, errors.Wrap(err, errors.KindInternal, "block.Explain", "unmarshal status")
	}
	return &st, nil
}

// sameBlockCounts converts node IDs to the per-node replica counts the
// S2 spread score compares against.
func sameBlockCounts(existing []string) map[string]int {
	m := map[string]int{}
	for _, id := range existing {
		m[id]++
	}
	return m
}

// RenderExplain renders an ExplainResponse in the spec §7 table shape.
// Human readability is a §10 exit-criteria item requiring manual review
// by someone who did not write the scheduler — the unit tests only pin
// structure (every node, specific reasons, score numbers).
func RenderExplain(r *pb.ExplainResponse) string {
	var sb strings.Builder
	head := fmt.Sprintf("Block %s/%s: %s", r.GetNamespace(), r.GetName(), r.GetPhase().String())
	if code := r.GetPendingReason().GetCode(); code != "" {
		head += " (" + code + ")"
	}
	sb.WriteString(head + "\n")
	for _, re := range r.GetReplicas() {
		sb.WriteString("\n")
		if re.GetNode() != "" {
			sb.WriteString(fmt.Sprintf("  Replica %d: placed on %s (%s)\n",
				re.GetReplicaIndex(), re.GetNode(), re.GetPhase().String()))
			continue
		}
		sb.WriteString(fmt.Sprintf("  Replica %d: Pending\n", re.GetReplicaIndex()))
		sb.WriteString("\n    Node        Filter result                                      Score\n")
		for _, n := range re.GetNodes() {
			if n.GetOk() {
				sb.WriteString(fmt.Sprintf("    %-11s %-50s %.0f (%s)\n",
					n.GetNode(), "✓", n.GetScore(), n.GetScoreDetail()))
			} else {
				sb.WriteString(fmt.Sprintf("    %-11s %-50s —\n",
					n.GetNode(), "✗ "+n.GetReason()))
			}
		}
		if sug := re.GetSuggestion(); sug != "" {
			sb.WriteString("\n    Suggestion: " + sug + "\n")
		}
	}
	return sb.String()
}
