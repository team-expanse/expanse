package api

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/expanse/expanse/internal/cluster/nodelc"
	"github.com/expanse/expanse/internal/store/raftstore"
	pb "github.com/expanse/expanse/proto"
)

// ---- node lifecycle (§4.8) through the running agent ----

func (s *Server) clusterStore(op string) (*raftstore.Store, error) {
	rs, ok := s.store.(*raftstore.Store)
	if !ok {
		return nil, status.Error(codes.FailedPrecondition, op+": not a cluster-mode agent")
	}
	return rs, nil
}

// ListNodes reports every enrolled node's lifecycle state.
func (s *Server) ListNodes(ctx context.Context, _ *pb.ListNodesRequest) (*pb.ListNodesResponse, error) {
	infos, err := nodelc.List(ctx, s.store)
	if err != nil {
		return nil, mapErr("ListNodes", err)
	}
	res := &pb.ListNodesResponse{}
	for _, in := range infos {
		res.Nodes = append(res.Nodes, &pb.ClusterNode{
			Id: in.ID, Role: in.Role, Lifecycle: in.Lifecycle, Cordoned: in.Cordoned,
			RaftAddr: in.RaftAddr, ApiAddr: in.APIAddr, LastSeenUnixNs: in.LastSeen.UnixNano(),
		})
	}
	return res, nil
}

// SetNodeCordon cordons or uncordons any node; followers forward the write.
func (s *Server) SetNodeCordon(ctx context.Context, req *pb.SetNodeCordonRequest) (*pb.SetNodeCordonResponse, error) {
	st, err := s.clusterStore("SetNodeCordon")
	if err != nil {
		return nil, err
	}
	set := nodelc.Uncordon
	if req.GetCordoned() {
		set = nodelc.Cordon
	}
	if err := set(ctx, st, req.GetNodeId()); err != nil {
		return nil, mapErr("SetNodeCordon", err)
	}
	return &pb.SetNodeCordonResponse{}, nil
}

// DrainNode cordons a node and counts the resources that must move off it.
func (s *Server) DrainNode(ctx context.Context, req *pb.DrainNodeRequest) (*pb.DrainNodeResponse, error) {
	st, err := s.clusterStore("DrainNode")
	if err != nil {
		return nil, err
	}
	n, err := nodelc.Drain(ctx, st, req.GetNodeId(), &nodelc.Options{IgnoreUnplaceable: req.GetIgnoreUnplaceable()})
	if err != nil {
		return nil, mapErr("DrainNode", err)
	}
	return &pb.DrainNodeResponse{Resources: int32(n)}, nil
}

// RemoveNode removes a node from raft and revokes its identity (leader only).
func (s *Server) RemoveNode(ctx context.Context, req *pb.RemoveNodeRequest) (*pb.RemoveNodeResponse, error) {
	st, err := s.clusterStore("RemoveNode")
	if err != nil {
		return nil, err
	}
	err = nodelc.Remove(ctx, st, req.GetNodeId(), &nodelc.Options{
		Force: req.GetForce(), Confirm: req.GetConfirm(), Reason: req.GetReason(), By: st.NodeID(),
	})
	if err != nil {
		return nil, mapErr("RemoveNode", err)
	}
	return &pb.RemoveNodeResponse{}, nil
}
