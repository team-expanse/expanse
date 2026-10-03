package api

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/expanse/expanse/internal/cluster/control"
	"github.com/expanse/expanse/internal/store/raftstore"
	pb "github.com/expanse/expanse/proto"
)

// ---- join tokens and CA rotation through the running agent ----

// enrolled returns the cluster store and identity, or FailedPrecondition.
func (s *Server) enrolled(op string) (*raftstore.Store, *ClusterIdentity, error) {
	st, err := s.clusterStore(op)
	if err != nil {
		return nil, nil, err
	}
	if s.Cluster == nil {
		return nil, nil, status.Error(codes.FailedPrecondition, op+": agent has no cluster identity")
	}
	return st, s.Cluster, nil
}

// CreateJoinToken mints a join token issued by this node.
func (s *Server) CreateJoinToken(ctx context.Context, req *pb.CreateJoinTokenRequest) (*pb.CreateJoinTokenResponse, error) {
	st, id, err := s.enrolled("CreateJoinToken")
	if err != nil {
		return nil, err
	}
	ttl := time.Duration(req.GetTtlSeconds()) * time.Second
	tok, err := control.CreateToken(ctx, st, id.ID, id.Secret, ttl, int(req.GetUses()), st.NodeID(), req.GetForNode())
	if err != nil {
		return nil, mapErr("CreateJoinToken", err)
	}
	return &pb.CreateJoinTokenResponse{Token: tok}, nil
}

// ListJoinTokens lists recorded join tokens, expired ones included.
func (s *Server) ListJoinTokens(ctx context.Context, _ *pb.ListJoinTokensRequest) (*pb.ListJoinTokensResponse, error) {
	st, err := s.clusterStore("ListJoinTokens")
	if err != nil {
		return nil, err
	}
	toks, err := control.ListTokens(ctx, st)
	if err != nil {
		return nil, mapErr("ListJoinTokens", err)
	}
	res := &pb.ListJoinTokensResponse{}
	for _, t := range toks {
		res.Tokens = append(res.Tokens, &pb.JoinToken{
			Nonce: t.Nonce, ExpiresUnixNs: t.Expires.UnixNano(), Uses: int32(t.Uses), Max: int32(t.Max), By: t.By,
		})
	}
	return res, nil
}

// RevokeJoinToken deletes a token by its full string or nonce.
func (s *Server) RevokeJoinToken(ctx context.Context, req *pb.RevokeJoinTokenRequest) (*pb.RevokeJoinTokenResponse, error) {
	st, id, err := s.enrolled("RevokeJoinToken")
	if err != nil {
		return nil, err
	}
	if err := control.RevokeToken(ctx, st, id.Secret, req.GetTokenOrNonce()); err != nil {
		return nil, mapErr("RevokeJoinToken", err)
	}
	return &pb.RevokeJoinTokenResponse{}, nil
}

// RotateCA starts a CA rotation; nodes renew onto the new CA on their own.
func (s *Server) RotateCA(ctx context.Context, _ *pb.RotateCARequest) (*pb.RotateCAResponse, error) {
	st, id, err := s.enrolled("RotateCA")
	if err != nil {
		return nil, err
	}
	if _, err := control.RotateCA(ctx, st, id.Secret, time.Now()); err != nil {
		return nil, mapErr("RotateCA", err)
	}
	return &pb.RotateCAResponse{}, nil
}

// GetCARotation reports rotation progress.
func (s *Server) GetCARotation(ctx context.Context, _ *pb.GetCARotationRequest) (*pb.GetCARotationResponse, error) {
	st, err := s.clusterStore("GetCARotation")
	if err != nil {
		return nil, err
	}
	rs, err := control.CARotationStatus(ctx, st)
	if err != nil {
		return nil, mapErr("GetCARotation", err)
	}
	return &pb.GetCARotationResponse{Rotating: rs.Rotating, Fingerprint: rs.Fingerprint, Pending: rs.Pending}, nil
}

// CompleteCARotation retires the outgoing CA once every node has renewed.
func (s *Server) CompleteCARotation(ctx context.Context, _ *pb.CompleteCARotationRequest) (*pb.CompleteCARotationResponse, error) {
	st, err := s.clusterStore("CompleteCARotation")
	if err != nil {
		return nil, err
	}
	if err := control.CompleteCARotation(ctx, st); err != nil {
		return nil, mapErr("CompleteCARotation", err)
	}
	return &pb.CompleteCARotationResponse{}, nil
}
