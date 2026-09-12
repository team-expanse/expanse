package join_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/cluster/ca"
	"github.com/expanse/expanse/internal/cluster/join"
	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/boltstore"
	"github.com/expanse/expanse/internal/store/raftstore"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

const testClusterID = "test-cluster-01234567"

// env is a minimal one-leader enrollment rig: a bootstrapped raftstore
// node, a cluster CA, a TLS JoinService endpoint, and helpers to create
// unbootstrapped joiner nodes (the state a joiner is in when it runs
// `cluster join`).
type env struct {
	t         *testing.T
	store     *raftstore.Store
	clusterCA *ca.CA
	secret    []byte
	joinAddr  string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	port := freeJoinPort(t)
	st, err := raftstore.Open(raftstore.Config{
		NodeID: "n0", BindAddr: fmt.Sprintf("127.0.0.1:%d", port),
		DataDir: t.TempDir(), Bootstrap: true,
	})
	if err != nil {
		t.Fatalf("open leader: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	waitJoinLeader(t, st, 10*time.Second)

	clusterCA, err := ca.Generate(testClusterID, time.Now())
	if err != nil {
		t.Fatalf("CA: %v", err)
	}
	secret := make([]byte, 32)
	copy(secret, []byte("0123456789abcdef0123456789abcdef"))

	// Leader's TLS identity for the join endpoint.
	leaderCert, priv, err := clusterCA.IssueNode("n0", "localhost", []net.IP{net.IPv4(127, 0, 0, 1)}, time.Now())
	if err != nil {
		t.Fatalf("issue leader cert: %v", err)
	}
	tlsCert := tls.Certificate{
		Certificate: [][]byte{leaderCert.Raw, clusterCA.Cert.Raw},
		PrivateKey:  priv,
	}
	bundle, err := ca.NewBundle(clusterCA.Cert)
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}

	svc := &join.Service{
		St: st, CA: clusterCA, ClusterID: testClusterID, Secret: secret, ThisNodeID: "n0",
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", freeJoinPort(t)))
	if err != nil {
		t.Fatalf("join listen: %v", err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(join.ServerTLSConfig(tlsCert, bundle))))
	svc.Register(srv)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)

	return &env{t: t, store: st, clusterCA: clusterCA, secret: secret, joinAddr: ln.Addr().String()}
}

// newJoiner creates an unbootstrapped raft node (listening, empty log).
func (e *env) newJoiner(id string) (string, error) {
	e.t.Helper()
	port := freeJoinPort(e.t)
	st, err := raftstore.Open(raftstore.Config{
		NodeID: id, BindAddr: fmt.Sprintf("127.0.0.1:%d", port), DataDir: e.t.TempDir(),
	})
	if err != nil {
		return "", err
	}
	e.t.Cleanup(func() { _ = st.Close() })
	return fmt.Sprintf("127.0.0.1:%d", port), nil
}

// joinCall issues one Join RPC as joiner `id`.
func (e *env) joinCall(ctx context.Context, id, raftAddr, token string) (*pb.JoinResponse, error) {
	e.t.Helper()
	csrDER, err := makeCSR(e.t, id)
	if err != nil {
		return nil, err
	}
	client := &join.Client{} // TOFU (no fingerprint in tests)
	return client.Join(ctx, e.joinAddr, &pb.JoinRequest{
		NodeId: id, Csr: csrDER, Token: token,
		AdvertiseAddr: raftAddr, ApiAddr: "127.0.0.1:1", Role: "voter",
	})
}

func (e *env) mintToken(ctx context.Context, uses int) string {
	e.t.Helper()
	tok, _, err := join.CreateToken(ctx, e.store, testClusterID, e.secret, time.Minute, uses, "n0")
	if err != nil {
		e.t.Fatalf("CreateToken: %v", err)
	}
	return tok
}

func (e *env) tokenUses(t *testing.T, token string) int {
	t.Helper()
	claims, err := join.ParseToken(token, e.secret)
	if err != nil {
		t.Fatalf("ParseToken: %v", err)
	}
	entry, err := e.store.Get(context.Background(), store.Key(join.TokenKeyPrefix)+store.Key(fmt.Sprintf("%x", claims.Nonce)))
	if err != nil {
		t.Fatalf("token record: %v", err)
	}
	var rec struct {
		Uses int `json:"uses"`
	}
	if err := jsonUnmarshal(entry.Value, &rec); err != nil {
		t.Fatalf("token record JSON: %v", err)
	}
	return rec.Uses
}

func grpcCodeIs(err error, want codes.Code) bool {
	s, ok := status.FromError(err)
	return ok && s.Code() == want
}

// --- tests ---------------------------------------------------------------

func TestTokenParseVerify(t *testing.T) {
	st, err := boltstore.New(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	secret := make([]byte, 32)

	tok, _, err := join.CreateToken(ctx, st, testClusterID, secret, time.Minute, 1, "n0")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	got, err := join.ParseToken(tok, secret)
	if err != nil {
		t.Fatalf("ParseToken: %v", err)
	}
	if got.ClusterIDPrefix != testClusterID[:8] || len(got.Nonce) != 8 {
		t.Errorf("claims = %+v", got)
	}
	if got.Expiry.Before(time.Now()) {
		t.Error("expiry in the past")
	}

	// Tamper: flip one base58 char.
	mutated := []byte(tok)
	if mutated[len(mutated)-1] == '2' {
		mutated[len(mutated)-1] = '3'
	} else {
		mutated[len(mutated)-1] = '2'
	}
	if _, err := join.ParseToken(string(mutated), secret); !errors.Is(err, errors.KindPermission) {
		t.Errorf("tampered token: err=%v, want permission", err)
	}

	// Wrong secret.
	other := make([]byte, 32)
	other[0] = 1
	if _, err := join.ParseToken(tok, other); !errors.Is(err, errors.KindPermission) {
		t.Errorf("wrong secret: err=%v, want permission", err)
	}

	// Expired.
	expired, _, err := join.CreateToken(ctx, st, testClusterID, secret, 10*time.Millisecond, 1, "n0")
	if err != nil {
		t.Fatalf("CreateToken expired: %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	if _, err := join.ParseToken(expired, secret); !errors.Is(err, errors.KindPermission) {
		t.Errorf("expired token: err=%v, want permission", err)
	}

	// Not a token.
	if _, err := join.ParseToken("garbage", secret); err == nil {
		t.Error("garbage accepted")
	}
}

func TestJoinEndToEnd(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	token := e.mintToken(ctx, 1)

	addr, err := e.newJoiner("n9")
	if err != nil {
		t.Fatalf("open joiner: %v", err)
	}
	resp, err := e.joinCall(ctx, "n9", addr, token)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if resp.GetClusterId() != testClusterID {
		t.Errorf("cluster id = %q", resp.GetClusterId())
	}
	if len(resp.GetClusterSecret()) != 32 || len(resp.GetPeers()) == 0 {
		t.Errorf("incomplete response: secret=%d peers=%d", len(resp.GetClusterSecret()), len(resp.GetPeers()))
	}

	// Node cert verifies against the returned CA.
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(resp.GetCaCert()) {
		t.Fatal("bad CA PEM in response")
	}
	cert, err := parseCertPEM(resp.GetNodeCert())
	if err != nil {
		t.Fatalf("node cert PEM: %v", err)
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool}); err != nil {
		t.Errorf("node cert does not verify: %v", err)
	}
	if cert.Subject.CommonName != "n9" {
		t.Errorf("CN = %q", cert.Subject.CommonName)
	}

	// /nodes/n9 recorded; token consumed.
	entry, err := e.store.Get(ctx, "/nodes/n9")
	if err != nil {
		t.Fatalf("node record: %v", err)
	}
	var rec join.NodeRecord
	if err := jsonUnmarshal(entry.Value, &rec); err != nil || rec.RaftAddr != addr {
		t.Errorf("node record = %s (err %v)", entry.Value, err)
	}
	if uses := e.tokenUses(t, token); uses != 1 {
		t.Errorf("token uses = %d, want 1", uses)
	}

	// Reuse of the single-use token is rejected.
	_, err = e.joinCall(ctx, "n8", "127.0.0.1:1", token)
	if !grpcCodeIs(err, codes.PermissionDenied) {
		t.Errorf("reuse: err=%v, want PermissionDenied", err)
	}
}

func TestRacingJoinsSameToken(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	token := e.mintToken(ctx, 1)

	addr, err := e.newJoiner("n9")
	if err != nil {
		t.Fatal(err)
	}

	type result struct {
		err error
	}
	ch := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, jerr := e.joinCall(ctx, "n9", addr, token)
			ch <- result{err: jerr}
		}()
	}
	var successes int
	for i := 0; i < 2; i++ {
		r := <-ch
		if r.err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("racing joins: %d succeeded; want exactly one", successes)
	}
	if uses := e.tokenUses(t, token); uses != 1 {
		t.Errorf("token uses = %d, want 1", uses)
	}
	if _, err := e.store.Get(ctx, "/nodes/n9"); err != nil {
		t.Fatalf("node record missing: %v", err)
	}
}

func TestJoinInterruptedReRun(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// First attempt completes; the "interruption" (joiner losing its
	// cert before persisting) is reproduced by re-running with a fresh
	// token — which must succeed and create no duplicate.
	token1 := e.mintToken(ctx, 1)
	addr, err := e.newJoiner("n10")
	if err != nil {
		t.Fatal(err)
	}
	resp1, err := e.joinCall(ctx, "n10", addr, token1)
	if err != nil {
		t.Fatalf("first join: %v", err)
	}

	token2 := e.mintToken(ctx, 1)
	resp2, err := e.joinCall(ctx, "n10", addr, token2)
	if err != nil {
		t.Fatalf("re-join: %v", err)
	}
	if string(resp1.GetClusterSecret()) != string(resp2.GetClusterSecret()) {
		t.Error("cluster secret changed between joins")
	}

	// Exactly one /nodes/n10 entry.
	entries, err := e.store.List(ctx, "/nodes/n10")
	if err != nil || len(entries) != 1 {
		t.Fatalf("node entries = %d, %v; want exactly 1", len(entries), err)
	}
}

func TestJoinNoQuorumFailFast(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// Joining through a NON-leader endpoint fails fast with Unavailable
	// and a leader_join hint (§10 "AddVoter while quorum unavailable":
	// never hang). Full quorum-loss timing is covered by the
	// raft-level tests in T07; here the fail-fast contract is the
	// non-leader path plus the joiner's bounded dial.
	svc := &join.Service{
		St: &fakeNotLeader{}, CA: e.clusterCA, ClusterID: testClusterID,
		Secret: e.secret, ThisNodeID: "n5",
		LeaderJoinAddr: func(raftAddr string) (string, bool) { return "127.0.0.1:7446", true },
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(insecure.NewCredentials()))
	svc.Register(srv)
	go func() { _ = srv.Serve(ln) }()
	defer srv.Stop()

	token := e.mintToken(ctx, 1)
	csrDER, err := makeCSR(t, "nx")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, jerr := (&join.Client{}).Join(context.Background(), ln.Addr().String(), &pb.JoinRequest{
		NodeId: "nx", Csr: csrDER, Token: token, AdvertiseAddr: "127.0.0.1:1",
	})
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("join hung %v; must fail fast", d)
	}
	if !grpcCodeIs(jerr, codes.Unavailable) {
		t.Errorf("err = %v, want Unavailable", jerr)
	}
	// The rejected join must not have consumed the token.
	if uses := e.tokenUses(t, token); uses != 0 {
		t.Errorf("token uses = %d after rejected join, want 0", uses)
	}
}

// fakeNotLeader satisfies join.Service's Store view of a follower.
type fakeNotLeader struct {
	raftstore.Store // nil-embedded: only IsLeader/Leader/AddVoter are called
}

func (f fakeNotLeader) IsLeader() bool              { return false }
func (f fakeNotLeader) AddVoter(_, _ string) error  { return nil }
func (f fakeNotLeader) RemoveServer(_ string) error { return nil }
func (f fakeNotLeader) Leader() string              { return "127.0.0.1:9999" }
