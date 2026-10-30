package service

import (
	"context"

	"github.com/expanse/expanse/internal/cluster/join"
	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/scheduler"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
)

// checkGrowthFits refuses next when a node already running cur can't hold its larger requests:
// an update restarts or resizes replicas where they are, so the scheduler never sees it.
func (s *Server) checkGrowthFits(ctx context.Context, cur, next *pb.Block) error {
	onNodes := liveNodes(cur.GetStatus())
	if s.Nodes == nil || len(onNodes) == 0 {
		return nil
	}
	nodes, cfg, err := s.Nodes(ctx)
	if err != nil {
		return errors.Wrap(err, errors.KindUnavailable, "blocks.service.Update", "cluster view for the new requests")
	}
	old, upd := scheduler.ReplicaRequest{Block: cur}, scheduler.ReplicaRequest{Block: next}
	for _, n := range nodes {
		if !onNodes[n.ID] {
			continue
		}
		n.ID = s.hostname(ctx, n.ID) // the message's name for it
		if why := scheduler.GrowthMisfit(n, old, upd, cfg); why != "" {
			return errors.New(errors.KindResourceExhausted, "blocks.service.Update",
				why+"; replicas update in place, so lower the requests or move the block first")
		}
	}
	return nil
}

// hostname is the name node published, or its ID when it published none.
func (s *Server) hostname(ctx context.Context, node string) string {
	if e, err := s.St.Get(store.WithStale(ctx), join.HostnameKey(node)); err == nil && len(e.Value) > 0 {
		return string(e.Value)
	}
	return node
}

// liveNodes is the set of nodes holding a live placement, the ones charged for the block's requests.
func liveNodes(st *pb.BlockStatus) map[string]bool {
	nodes := map[string]bool{}
	for _, p := range st.GetPlacements() {
		if p.GetReplicaIndex() >= 0 && p.GetPhase() != pb.Phase_LOST {
			nodes[p.GetNodeId()] = true
		}
	}
	return nodes
}
