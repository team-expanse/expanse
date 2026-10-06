package join

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"

	"github.com/expanse/expanse/internal/errors"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// Client is the joiner side of the §4.5 protocol.
type Client struct {
	// CAFingerprint, when non-empty (hex SHA-256 of the CA cert DER),
	// pins the join endpoint's CA — use it whenever the operator has
	// the fingerprint out-of-band. When empty the first join is
	// trust-on-first-use: the CA returned by the leader is trusted and
	// must be persisted (pinned) before the node serves anything.
	CAFingerprint string
}

// ClientTLSConfig builds the join TLS configuration. With a pinned
// fingerprint the chain is verified against that CA; without it the
// first connection is TOFU (documented hazard — the token is short-lived
// and the enrollment network is assumed administrative).
func (c *Client) ClientTLSConfig() *tls.Config {
	fp := c.CAFingerprint
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, //nolint:gosec // verified in VerifyPeerCertificate when pinned
		// No ClientSessionCache: every join is a full handshake, so this callback always runs.
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error { //nolint:gosec // see above
			if fp == "" {
				return nil // TOFU
			}
			for _, raw := range rawCerts {
				sum := sha256.Sum256(raw)
				if hex.EncodeToString(sum[:]) == fp {
					return nil
				}
			}
			return errors.New(errors.KindPermission, "join.Client", "CA fingerprint mismatch")
		},
	}
}

// Join dials addr (host:7446) and enrolls. If the contacted node is not
// the leader, its Unavailable detail carries "leader_join=<addr>" and
// the client retries there once.
func (c *Client) Join(ctx context.Context, addr string, req *pb.JoinRequest) (*pb.JoinResponse, error) {
	resp, err := c.dialJoin(ctx, addr, req)
	if err == nil {
		return resp, nil
	}
	if next, ok := redirectAddr(err); ok {
		return c.dialJoin(ctx, next, req)
	}
	return nil, err
}

func (c *Client) dialJoin(ctx context.Context, addr string, req *pb.JoinRequest) (*pb.JoinResponse, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(c.ClientTLSConfig())))
	if err != nil {
		return nil, errors.Wrap(err, errors.KindUnavailable, "join.Client", "dial: "+err.Error())
	}
	defer conn.Close()
	resp, err := pb.NewJoinServiceClient(conn).Join(ctx, req)
	if err != nil {
		return nil, err // gRPC status carries the redirect detail
	}
	if len(resp.GetCaCert()) == 0 || len(resp.GetNodeCert()) == 0 || len(resp.GetClusterSecret()) != 32 {
		return nil, errors.New(errors.KindInternal, "join.Client", "incomplete JoinResponse")
	}
	return resp, nil
}

// redirectAddr extracts leader_join=<addr> from an Unavailable error.
func redirectAddr(err error) (string, bool) {
	s, ok := status.FromError(err)
	if !ok {
		return "", false
	}
	const tag = "leader_join="
	msg := s.Message()
	i := indexOf(msg, tag)
	if i < 0 {
		return "", false
	}
	rest := msg[i+len(tag):]
	for j := 0; j < len(rest); j++ {
		if rest[j] == ' ' {
			return rest[:j], true
		}
	}
	return rest, true
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
