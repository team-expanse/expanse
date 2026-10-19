package service

import (
	"context"
	"sort"
	"strings"

	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/scheduler"
	expstorage "github.com/expanse/expanse/internal/storage"
	pb "github.com/expanse/expanse/proto"
)

// Planned is where a pre-flight would place one replica, or each node's reason for refusing it.
type Planned struct {
	Replica    int // -1 for a DAEMONSET, which runs one per fitting node
	Node       string
	Rejected   map[string]string // node → "Code: detail", when Node is empty
	Code       string            // the reason every node gave, when they agree
	Suggestion string
}

// Preflight places b on the current cluster view as the controller would, without storing anything.
func (s *Server) Preflight(ctx context.Context, b *pb.Block) ([]Planned, error) {
	if s.Nodes == nil {
		return nil, errors.New(errors.KindUnavailable, "block.Preflight", "no cluster view configured")
	}
	nodes, cfg, err := s.Nodes(ctx)
	if err != nil {
		return nil, errors.Wrap(err, errors.KindUnavailable, "block.Preflight", "cluster view")
	}
	nodes = withOwnVolumes(nodes, b)
	if b.GetSpec().GetStrategy().GetKind() == pb.StrategyKind_DAEMONSET {
		return preflightDaemonset(nodes, b, cfg), nil
	}
	var plan []Planned
	var existing []string
	for i := 0; i < desiredReplicas(b, nodes); i++ {
		req := scheduler.ReplicaRequest{Block: b, ReplicaIndex: i, ExistingPlacements: existing}
		cands, reasons := scheduler.Filter(nodes, req, cfg)
		if len(cands) == 0 {
			plan = append(plan, unplaced(i, reasons))
			continue
		}
		node := scheduler.Score(cands, req, scheduler.ClusterView{SameBlockReplicas: sameBlockCounts(existing)})[0].NodeID
		scheduler.Reserve(nodes, node, req)
		existing = append(existing, node)
		plan = append(plan, Planned{Replica: i, Node: node})
	}
	return plan, nil
}

func preflightDaemonset(nodes []scheduler.NodeView, b *pb.Block, cfg scheduler.OvercommitConfig) []Planned {
	cands, reasons := scheduler.Filter(nodes, scheduler.ReplicaRequest{Block: b}, cfg)
	if len(cands) == 0 {
		return []Planned{unplaced(-1, reasons)}
	}
	plan := make([]Planned, 0, len(cands))
	for _, n := range cands {
		plan = append(plan, Planned{Replica: -1, Node: n.ID})
	}
	sort.Slice(plan, func(i, j int) bool { return plan[i].Node < plan[j].Node })
	return plan
}

// unplaced explains a replica no node takes, with a hint when every node refused for one reason.
func unplaced(i int, reasons map[string]string) Planned {
	p := Planned{Replica: i, Rejected: reasons}
	if len(reasons) == 0 {
		p.Code, p.Suggestion = scheduler.CodeNoNodes, suggestionFor(scheduler.CodeNotReady)
		return p
	}
	code := ""
	for _, r := range reasons {
		c, _, _ := strings.Cut(r, ": ")
		if code != "" && c != code {
			return p
		}
		code = c
	}
	p.Code, p.Suggestion = code, suggestionFor(code)
	return p
}

// withOwnVolumes copies nodes, counting b's volumes as healthy on each: a new block's volumes
// are made where it is placed, so P12 must not refuse it for lacking them.
func withOwnVolumes(nodes []scheduler.NodeView, b *pb.Block) []scheduler.NodeView {
	ns, name := b.GetMetadata().GetNamespace(), b.GetMetadata().GetName()
	out := make([]scheduler.NodeView, len(nodes))
	for i, n := range nodes {
		n.HealthyVolumes = append([]string(nil), n.HealthyVolumes...)
		for _, st := range b.GetSpec().GetStorage() {
			n.HealthyVolumes = append(n.HealthyVolumes, expstorage.BlockVolumeName(ns, name, st.GetName()))
		}
		out[i] = n
	}
	return out
}
