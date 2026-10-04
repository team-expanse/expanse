package join

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"time"

	"github.com/expanse/expanse/internal/cluster/ca"
	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Service is the leader-side enrollment endpoint (§4.5). Serve it on
// :7446 behind the cluster CA's TLS. The leader performs all state
// changes; a non-leader instance returns Unavailable together with the
// leader's address so the client can retry there (fail-fast — §10
// "AddVoter while quorum unavailable": a leaderless or quorum-less
// cluster rejects joins immediately instead of hanging).
type Service struct {
	pb.UnimplementedJoinServiceServer

	St         Store
	CA         *ca.CA
	ClusterID  string
	Secret     []byte // 32 bytes
	ThisNodeID string

	// CASource, when set, supplies the CA new joiners are signed under
	// on every call, overriding the static CA field — so a leader
	// mid-rotation (Phase 10 X2) signs joiners with the current
	// primary CA, not whatever was loaded at startup. CA remains the
	// fallback (and the only source SealedCAKey caching applies to).
	CASource func() (*ca.CA, error)

	// SealedCAKey is the CA private key age-sealed to the cluster
	// secret (§4.4 layout: every node stores it so any node can serve
	// the join endpoint when it becomes leader). Computed lazily from
	// CA.Priv + Secret when nil. Only used for the static CA field;
	// CASource callers reseal on every call, since the key can change
	// across rotation.
	SealedCAKey []byte

	// LeaderJoinAddr maps a leader raft address to the :7446 join
	// endpoint advertised to redirected clients. Optional.
	LeaderJoinAddr func(raftAddr string) (string, bool)
}

// Store is the raft-backed store surface the join service needs: linear
// store operations (through the leader) plus Raft membership changes.
type Store interface {
	store.Store
	IsLeader() bool
	AddVoter(id, addr string) error
	Leader() string
}

// Register wires the service into a gRPC server.
func (s *Service) Register(srv *grpc.Server) {
	pb.RegisterJoinServiceServer(srv, s)
}

// ServerTLSConfig returns TLS for :7446: the node's CA-signed cert, no
// client auth (joiners have no credentials yet — the token authorizes).
func ServerTLSConfig(cert tls.Certificate, caBundle *ca.Bundle) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.NoClientCert,
		MinVersion:   tls.VersionTLS13,
		RootCAs:      caBundle.Pool(),
	}
}

func joinErr(c codes.Code, err error) error {
	return status.Error(c, err.Error())
}

// activeCA resolves the CA new joiners are signed under: CASource when
// set (the live, possibly-rotated primary), else the static CA field.
func (s *Service) activeCA() (*ca.CA, error) {
	if s.CASource != nil {
		return s.CASource()
	}
	return s.CA, nil
}

// Join implements the §4.5 protocol:
//
//	verify token (unused, unexpired, HMAC valid, cluster match)
//	→ single Raft txn: consume token + write /nodes/<id> (CAS on
//	  token record; node key must-not-exist unless idempotent re-join)
//	→ raft.AddVoter (idempotent; compensating rollback on failure)
//	→ sign CSR → respond with cluster material
func (s *Service) Join(ctx context.Context, req *pb.JoinRequest) (*pb.JoinResponse, error) {
	if req.GetNodeId() == "" || req.GetAdvertiseAddr() == "" || len(req.GetCsr()) == 0 {
		return nil, joinErr(codes.InvalidArgument, errors.New(errors.KindInvalid, "join.Join", "node_id, advertise_addr and csr are required"))
	}
	if !s.St.IsLeader() {
		lead := s.St.Leader()
		hint := ""
		if s.LeaderJoinAddr != nil {
			if j, ok := s.LeaderJoinAddr(lead); ok {
				hint = " leader_join=" + j
			}
		}
		return nil, status.Errorf(codes.Unavailable, "join: not leader%s", hint)
	}

	// Revoked identity (§4.8): a removed node's ID is permanently
	// rejected, regardless of token validity — checked before anything
	// else so a revoked re-join can never consume a token. Fails
	// CLOSED: only a confirmed "not found" means "not revoked" (mirrors
	// nodelc.IsRevoked); any other store error (a transient Raft
	// forwarding/leader-election hiccup, say) must not be silently
	// treated as "proceed with the join" — a security review finding
	// (Phase 10 X1) flagged the original fail-open form of this check.
	switch _, err := s.St.Get(ctx, store.Key(RevokedKeyPrefix+req.GetNodeId())); {
	case err == nil:
		return nil, joinErr(codes.PermissionDenied, errors.New(errors.KindPermission, "join.Join",
			"node "+req.GetNodeId()+" was removed from this cluster; its identity is revoked"))
	case errors.Is(err, errors.KindNotFound):
		// not revoked; continue
	default:
		return nil, joinErr(codes.Unavailable, errors.Wrap(err, errors.KindUnavailable, "join.Join",
			"revocation check failed: "+err.Error()))
	}

	// 1. Token: HMAC + expiry + cluster binding.
	claims, err := ParseToken(req.GetToken(), s.Secret)
	if err != nil {
		return nil, joinErr(codes.PermissionDenied, err)
	}
	if claims.ClusterIDPrefix != s.ClusterID[:8] {
		return nil, joinErr(codes.PermissionDenied, errors.New(errors.KindPermission, "join.Join", "token minted for a different cluster"))
	}
	tokKey := store.Key(TokenKeyPrefix + hexEncode(claims.Nonce))
	tokEntry, err := s.St.Get(ctx, tokKey)
	if err != nil {
		return nil, joinErr(codes.PermissionDenied, errors.New(errors.KindPermission, "join.Join", "unknown token (never issued)"))
	}
	var rec tokenRecord
	if err := json.Unmarshal(tokEntry.Value, &rec); err != nil {
		return nil, joinErr(codes.Internal, errors.New(errors.KindInternal, "join.Join", "corrupt token record"))
	}
	if rec.Uses >= rec.MaxUses {
		return nil, joinErr(codes.PermissionDenied, errors.New(errors.KindPermission, "join.Join", "token exhausted"))
	}

	// 2. CSR: parse, verify signature, CN binding.
	csr, err := x509.ParseCertificateRequest(req.GetCsr())
	if err != nil {
		return nil, joinErr(codes.InvalidArgument, errors.New(errors.KindInvalid, "join.Join", "bad CSR: "+err.Error()))
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, joinErr(codes.InvalidArgument, errors.New(errors.KindInvalid, "join.Join", "CSR signature invalid"))
	}
	if csr.Subject.CommonName != req.GetNodeId() {
		return nil, joinErr(codes.InvalidArgument, errors.New(errors.KindInvalid, "join.Join", "CSR CN must equal node_id"))
	}
	// SANs are signed verbatim by ca.IssueCSR (it trusts its caller), so
	// this is the only place that can validate them. The legitimate
	// client (control.Enroll) only ever requests {node_id, UIVIPHostname}
	// with no IP SANs — anything else is a forged-identity attempt, not
	// a real joiner (security review finding, Phase 10 X1).
	for _, name := range csr.DNSNames {
		if name != req.GetNodeId() && name != ca.UIVIPHostname {
			return nil, joinErr(codes.InvalidArgument, errors.New(errors.KindInvalid, "join.Join",
				"CSR requests an unrecognized DNS SAN: "+name))
		}
	}
	if len(csr.IPAddresses) > 0 {
		return nil, joinErr(codes.InvalidArgument, errors.New(errors.KindInvalid, "join.Join", "CSR must not request IP SANs"))
	}
	fp, err := PubKeyFingerprint(csr.PublicKey)
	if err != nil {
		return nil, joinErr(codes.InvalidArgument, errors.New(errors.KindInvalid, "join.Join", "CSR public key: "+err.Error()))
	}

	// 3. Idempotent re-join: /nodes/<id> may already exist from an
	// interrupted first attempt — that must succeed, not duplicate. But
	// it is only AUTHORIZED as a re-join, rather than a hijack of
	// someone else's identity, if the token presented was deliberately
	// scoped to this exact node_id (rec.ForNodeID, checked in step 4
	// below) — node_id alone is client-supplied and proves nothing
	// (security review finding, Phase 10 X1: the original check trusted
	// node_id alone, so any valid token — e.g. a broadly-distributed
	// bulk-provisioning one — could claim an arbitrary existing node's
	// identity). The CSR's key is deliberately NOT required to match
	// the original: a legitimate re-join can follow the joiner losing
	// its key material (TestJoinInterruptedReRun), so key continuity
	// can't be the authorization boundary — token scoping, which only
	// an operator who already trusts this is a genuine recovery can
	// grant, is.
	nodesKey := store.Key(NodesKeyPrefix + req.GetNodeId())
	existing, gerr := s.St.Get(ctx, nodesKey)
	rejoin := gerr == nil
	if rejoin && rec.ForNodeID != req.GetNodeId() {
		return nil, joinErr(codes.AlreadyExists, errors.New(errors.KindConflict, "join.Join",
			"node_id "+req.GetNodeId()+" is already enrolled; recovering it requires a token minted with --for-node "+req.GetNodeId()))
	}

	// 4. ONE Raft txn: consume token + node record. Racing joins with
	// the same single-use token serialize here; the loser gets a
	// conflict on OpCheck{token record}.
	consumed, err := json.Marshal(func() tokenRecord {
		rec.Uses++
		return rec
	}())
	nodeRecord := NodeRecord{
		ID: req.GetNodeId(), RaftAddr: req.GetAdvertiseAddr(),
		APIAddr: req.GetApiAddr(), JoinedAt: time.Now().UnixNano(),
		Inventory: req.GetInventory(), PubKeyFingerprint: fp,
	}
	if req.GetRole() != "" {
		nodeRecord.Role = req.GetRole()
	}
	nr, err2 := json.Marshal(nodeRecord)
	if err2 != nil || err != nil {
		return nil, joinErr(codes.Internal, errors.New(errors.KindInternal, "join.Join", "encode"))
	}
	ops := []store.Op{
		{Kind: store.OpCheck, Key: tokKey, Expect: tokEntry.Revision},
		{Kind: store.OpPut, Key: tokKey, Value: consumed},
	}
	if rejoin {
		ops = append(ops, store.Op{Kind: store.OpCheck, Key: nodesKey, Expect: existing.Revision})
		ops = append(ops, store.Op{Kind: store.OpPut, Key: nodesKey, Value: nr})
	} else {
		ops = append(ops, store.Op{Kind: store.OpCheck, Key: nodesKey, Expect: 0}) // must not exist
		ops = append(ops, store.Op{Kind: store.OpPut, Key: nodesKey, Value: nr})
	}
	if _, err := s.St.Txn(ctx, ops); err != nil {
		return nil, joinErr(codes.FailedPrecondition, errors.Wrap(err, errors.KindConflict, "join.Join",
			"token race or duplicate node: "+err.Error()))
	}

	// 5. Raft membership. AddVoter is idempotent (same ID+addr is a
	// no-op address update), which is what makes the interrupted-join
	// re-run safe. Fail-fast: a lost quorum fails within ~1 election
	// timeout (hashicorp raft returns ErrLeadershipLost), not a hang.
	if err := s.St.AddVoter(req.GetNodeId(), req.GetAdvertiseAddr()); err != nil {
		s.rollback(ctx, tokKey, tokEntry.Revision, nodesKey, existing, rejoin)
		return nil, joinErr(codes.Unavailable, errors.Wrap(err, errors.KindUnavailable, "join.Join",
			"AddVoter failed (no quorum or lost leadership): "+err.Error()))
	}

	// 6. Sign the joiner's CSR (key generated and kept by the joiner;
	// only the public key travels). Re-join deliberately re-signs: the
	// interrupted joiner may have lost its key material. Signs under
	// the current primary CA (activeCA), which moves mid-rotation
	// (Phase 10 X2) so new joiners never land on a CA about to be
	// retired.
	activeCA, err := s.activeCA()
	if err != nil {
		return nil, joinErr(codes.Unavailable, errors.Wrap(err, errors.KindUnavailable, "join.Join", "load signing CA: "+err.Error()))
	}
	cert, err := activeCA.IssueCSR(csr, time.Now())
	if err != nil {
		return nil, joinErr(codes.Internal, errors.Wrap(err, errors.KindInternal, "join.Join", "sign CSR: "+err.Error()))
	}
	certPEM := ca.MarshalCert(cert)

	// 7. Cluster material + current membership.
	peers, err := s.peerList(ctx)
	if err != nil {
		return nil, joinErr(codes.Internal, err)
	}
	return &pb.JoinResponse{
		CaCert:         ca.MarshalCert(activeCA.Cert),
		NodeCert:       certPEM,
		ClusterId:      s.ClusterID,
		ClusterSecret:  append([]byte(nil), s.Secret...),
		Peers:          peers,
		RaftConfig:     `{"heartbeat":1000,"election":1000}`, // ms, matches raftstore defaults
		LeaderRaftAddr: s.St.Leader(),
		SealedCaKey:    s.sealedCAKey(activeCA),
	}, nil
}

// rollback compensates a failed AddVoter: release the token consumption
// and remove a node record we created, restoring the pre-join state.
func (s *Service) rollback(ctx context.Context, tokKey store.Key, tokRev store.Revision, nodesKey store.Key, existing *store.Entry, rejoin bool) {
	var ops []store.Op
	if rejoin {
		ops = append(ops, store.Op{Kind: store.OpCheck, Key: nodesKey, Expect: existing.Revision})
		ops = append(ops, store.Op{Kind: store.OpPut, Key: nodesKey, Value: existing.Value})
	} else {
		cur, err := s.St.Get(ctx, nodesKey)
		if err == nil {
			ops = append(ops, store.Op{Kind: store.OpDelete, Key: nodesKey, Expect: cur.Revision})
		}
	}
	if cur, err := s.St.Get(ctx, tokKey); err == nil {
		var rec tokenRecord
		if json.Unmarshal(cur.Value, &rec) == nil && rec.Uses > 0 {
			rec.Uses--
			if v, err := json.Marshal(rec); err == nil {
				ops = append(ops, store.Op{Kind: store.OpCheck, Key: tokKey, Expect: cur.Revision})
				ops = append(ops, store.Op{Kind: store.OpPut, Key: tokKey, Value: v})
			}
		}
	}
	if len(ops) > 0 {
		_, _ = s.St.Txn(ctx, ops) // best effort; token TTL bounds any leak
	}
}

func (s *Service) peerList(ctx context.Context) ([]*pb.JoinPeer, error) {
	entries, err := s.St.List(ctx, NodesKeyPrefix)
	if err != nil {
		return nil, joinErr(codes.Unavailable, errors.Wrap(err, errors.KindUnavailable, "join.peerList", err.Error()))
	}
	peers := make([]*pb.JoinPeer, 0, len(entries))
	for _, e := range entries {
		var r NodeRecord
		if err := json.Unmarshal(e.Value, &r); err != nil {
			continue
		}
		peers = append(peers, &pb.JoinPeer{
			Id: r.ID, RaftAddr: r.RaftAddr, ApiAddr: r.APIAddr, Role: r.Role,
		})
	}
	return peers, nil
}

func hexEncode(b []byte) string {
	return fmt.Sprintf("%x", b)
}

// sealedCAKey returns the age-sealed CA private key (sealed to the
// cluster secret) for activeCA, sealing on first use when CASource is
// unset (the static, non-rotating case). With CASource set, the CA can
// change across calls, so the cache is bypassed and every call reseals.
// The joiner stores it verbatim; its exposure equals the cluster secret
// it is sealed to.
func (s *Service) sealedCAKey(activeCA *ca.CA) []byte {
	if s.CASource != nil {
		sealed, err := ca.SealKey(activeCA.Priv, s.Secret)
		if err != nil {
			return nil
		}
		return sealed
	}
	if len(s.SealedCAKey) == 0 {
		sealed, err := ca.SealKey(s.CA.Priv, s.Secret)
		if err != nil {
			return nil
		}
		s.SealedCAKey = sealed
	}
	return s.SealedCAKey
}

// Removal tells a node whether it was removed, from a linearizable read: only
// a peer with quorum answers, so a stale "no" can never come back.
func (s *Service) Removal(ctx context.Context, req *pb.RemovalRequest) (*pb.RemovalResponse, error) {
	if req.GetNodeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "join.Removal: node_id is required")
	}
	switch e, err := s.St.Get(ctx, store.Key(RevokedKeyPrefix+req.GetNodeId())); {
	case err == nil:
		return &pb.RemovalResponse{Removed: true, Revocation: e.Value}, nil
	case errors.Is(err, errors.KindNotFound):
		return &pb.RemovalResponse{}, nil
	default:
		return nil, status.Error(codes.Unavailable, "join.Removal: "+err.Error())
	}
}
