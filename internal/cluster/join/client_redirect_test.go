package join_test

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"net"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/cluster/ca"
	"github.com/expanse/expanse/internal/cluster/join"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// tlsStub serves a gRPC server with a fresh cluster-CA-signed identity.
func tlsStub(t *testing.T, stub pb.JoinServiceServer) string {
	t.Helper()
	clusterCA, err := ca.Generate("redirect-test", time.Now())
	if err != nil {
		t.Fatalf("ca.Generate: %v", err)
	}
	cert, priv, err := clusterCA.IssueNode("stub", "localhost", []net.IP{net.IPv4(127, 0, 0, 1)}, time.Now())
	if err != nil {
		t.Fatalf("IssueNode: %v", err)
	}
	pair := tls.Certificate{Certificate: [][]byte{cert.Raw, clusterCA.Cert.Raw}, PrivateKey: priv}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{pair},
	})))
	pb.RegisterJoinServiceServer(srv, stub)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	return ln.Addr().String()
}

// TestClientFollowsRedirect: a non-leader join endpoint answers
// Unavailable with a leader_join hint; the client retries there.
func TestClientFollowsRedirect(t *testing.T) {
	leaderAddr := tlsStub(t, &stubJoin{resp: &pb.JoinResponse{CaCert: []byte("ca"), NodeCert: []byte("c"), ClusterSecret: make([]byte, 32)}})
	followerAddr := tlsStub(t, &stubJoin{
		err: status.Errorf(codes.Unavailable, "join: not leader leader_join=%s", leaderAddr),
	})

	cli := &join.Client{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := cli.Join(ctx, followerAddr, &pb.JoinRequest{
		NodeId: "nx", AdvertiseAddr: "127.0.0.1:1", Csr: []byte("csr"),
	})
	if err != nil {
		t.Fatalf("Join via redirect: %v", err)
	}
	if len(resp.GetNodeCert()) == 0 {
		t.Error("empty node cert after redirect")
	}

	// No redirect on a plain error: the failure surfaces.
	badAddr := tlsStub(t, &stubJoin{err: status.Error(codes.PermissionDenied, "bad token")})
	if _, err := cli.Join(ctx, badAddr, &pb.JoinRequest{NodeId: "nx"}); err == nil {
		t.Error("permission-denied join accepted")
	}
}

// stubJoin answers with a canned response or error.
type stubJoin struct {
	pb.UnimplementedJoinServiceServer
	resp *pb.JoinResponse
	err  error
}

func (s *stubJoin) Join(ctx context.Context, req *pb.JoinRequest) (*pb.JoinResponse, error) {
	return s.resp, s.err
}

// TestClientTLSConfigPinning covers TOFU (no fingerprint), the matching
// fingerprint, and the mismatch rejection.
func TestClientTLSConfigPinning(t *testing.T) {
	fake := []byte{0x01, 0x02, 0x03}
	sum := sha256.Sum256(fake)
	fp := hex.EncodeToString(sum[:])

	// TOFU: no pin, verifier is a no-op.
	cfg := (&join.Client{}).ClientTLSConfig()
	if err := cfg.VerifyPeerCertificate([][]byte{fake}, nil); err != nil {
		t.Errorf("TOFU rejected: %v", err)
	}

	// Matching pin.
	cfg = (&join.Client{CAFingerprint: fp}).ClientTLSConfig()
	if err := cfg.VerifyPeerCertificate([][]byte{fake}, nil); err != nil {
		t.Errorf("matching pin rejected: %v", err)
	}

	// Mismatched pin.
	cfg = (&join.Client{CAFingerprint: "deadbeef"}).ClientTLSConfig()
	if err := cfg.VerifyPeerCertificate([][]byte{fake}, nil); err == nil {
		t.Error("fingerprint mismatch accepted")
	}
}

// TestJoinResponseValidation covers the incomplete-response guard.
func TestJoinResponseValidation(t *testing.T) {
	addr := tlsStub(t, &stubJoin{resp: &pb.JoinResponse{CaCert: []byte("ca")}}) // missing cert+secret
	cli := &join.Client{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := cli.Join(ctx, addr, &pb.JoinRequest{NodeId: "nx"}); err == nil {
		t.Error("incomplete JoinResponse accepted")
	}
}
