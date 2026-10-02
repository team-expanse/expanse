package service

import (
	"context"

	"github.com/expanse/expanse/internal/blocks/health"
	pb "github.com/expanse/expanse/proto"
)

// withHealth attaches each live placement's probe records, read only from the node it runs on,
// and counts the RUNNING replicas whose readiness is not failing as ready.
func (s *Server) withHealth(ctx context.Context, ns, name string, st *pb.BlockStatus) {
	var ready int32
	for _, p := range st.GetPlacements() {
		if p.GetReplicaIndex() < 0 {
			continue
		}
		r, l, err := health.RecordsOf(ctx, s.St, key(ns, name), p.GetReplicaIndex(), p.GetNodeId())
		if err == nil { // unreadable is unknown
			p.Health = replicaHealth(r, l)
		}
		if p.GetPhase() == pb.Phase_RUNNING && (r == nil || r.OK) {
			ready++
		}
	}
	if st.Replicas == nil {
		st.Replicas = &pb.StatusReplicas{}
	}
	st.Replicas.Ready = ready
}

func replicaHealth(ready *health.Record, live *health.LivenessRecord) *pb.ReplicaHealth {
	h := &pb.ReplicaHealth{}
	if ready != nil {
		h.Readiness = &pb.ProbeResult{Ok: ready.OK, Detail: ready.Detail, AtUnixNs: ready.At.UnixNano()}
	}
	if live != nil {
		h.Liveness = &pb.ProbeResult{Ok: !live.Failed, Detail: live.Detail, AtUnixNs: live.At.UnixNano()}
		h.Restarts = int32(live.Restarts)
	}
	return h
}
