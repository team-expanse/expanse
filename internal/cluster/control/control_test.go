package control_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/cluster/ca"
	"github.com/expanse/expanse/internal/cluster/control"
	"github.com/expanse/expanse/internal/cluster/join"
	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/raftstore"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// rig is a complete in-proc enrollment environment: one bootstrapped
// leader with its TLS join endpoint, plus joiner data dirs on free ports.
type rig struct {
	t        *testing.T
	dataDir  string
	initRes  *control.InitResult
	joinAddr string
	srv      *grpc.Server
}

func newRig(t *testing.T, name string) *rig {
	t.Helper()
	dataDir := t.TempDir()
	// A free port for the leader's raft transport.
	leaderRaft := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	res, err := control.Init(context.Background(), control.InitOptions{
		DataDir: dataDir, NodeID: "n1", Name: name,
		AdvertiseAddr: leaderRaft, BindAddr: leaderRaft,
		JoinHost: "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = res.Store.Close() })

	// TLS join endpoint on the leader, mirroring the daemon wiring.
	pair, err := tlsKeyPair(t, dataDir)
	if err != nil {
		t.Fatalf("leader keypair: %v", err)
	}
	bundle, err := ca.NewBundle(res.CA.Cert)
	if err != nil {
		t.Fatal(err)
	}
	svc := &join.Service{St: res.Store, CA: res.CA, ClusterID: res.ClusterID, Secret: res.Secret, ThisNodeID: "n1"}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(join.ServerTLSConfig(pair, bundle))))
	svc.Register(srv)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)

	return &rig{t: t, dataDir: dataDir, initRes: res, joinAddr: ln.Addr().String(), srv: srv}
}

// tlsKeyPair issues the leader's TLS identity from the rig's CA.
func tlsKeyPair(t *testing.T, dataDir string) (tls.Certificate, error) {
	t.Helper()
	_, _, clusterCA, err := control.LoadCluster(dataDir)
	if err != nil {
		return tls.Certificate{}, err
	}
	cert, priv, err := clusterCA.IssueNode("n1", "localhost", []net.IP{net.IPv4(127, 0, 0, 1)}, time.Now())
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{cert.Raw, clusterCA.Cert.Raw}, PrivateKey: priv}, nil
}

// enrollNode runs the full joiner flow (control.Enroll) against the rig.
func (r *rig) enrollNode(id string, role string) *control.EnrollResult {
	r.t.Helper()
	raftPort := freePort(r.t)
	// control.Enroll binds its own raft transport; pin to 127.0.0.1.
	dir := r.t.TempDir()
	res, err := control.Enroll(context.Background(), control.EnrollOptions{
		DataDir: dir, NodeID: id, Address: r.joinAddr,
		Token: r.mintToken(), Role: role, APIAddr: "127.0.0.1:1",
		// BindAddr/AdvertiseAddr defaults use 0.0.0.0/localIP; tests
		// pin both to loopback with explicit ports.
		BindAddr:      fmt.Sprintf("127.0.0.1:%d", raftPort),
		AdvertiseAddr: fmt.Sprintf("127.0.0.1:%d", raftPort),
	})
	if err != nil {
		r.t.Fatalf("Enroll %s: %v", id, err)
	}
	r.t.Cleanup(func() { _ = res.Store.Close() })
	return res
}

func (r *rig) mintToken() string {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tok, err := control.CreateToken(ctx, r.initRes.Store, r.initRes.ClusterID, r.initRes.Secret, time.Minute, 1, "n1", "")
	if err != nil {
		r.t.Fatalf("CreateToken: %v", err)
	}
	return tok
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestInitCreatesCluster(t *testing.T) {
	dir := t.TempDir()
	res, err := control.Init(context.Background(), control.InitOptions{
		DataDir: dir, NodeID: "n1", Name: "test", AdvertiseAddr: fmt.Sprintf("127.0.0.1:%d", freePort(t)),
		BindAddr: fmt.Sprintf("127.0.0.1:%d", freePort(t)),
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer res.Store.Close()

	if res.ClusterID == "" || len(res.Secret) != 32 || res.Token == "" {
		t.Fatalf("incomplete init result")
	}
	if got, _ := join.ParseToken(res.Token, res.Secret); got.ClusterIDPrefix != res.ClusterID[:8] {
		t.Error("token not bound to cluster")
	}

	// Persisted enrollment state.
	if !control.IsClusterNode(dir) {
		t.Error("cluster-id file missing")
	}
	id, secret, clusterCA, err := control.LoadCluster(dir)
	if err != nil {
		t.Fatalf("LoadCluster: %v", err)
	}
	if id != res.ClusterID || string(secret) != string(res.Secret) || clusterCA == nil {
		t.Error("persisted enrollment mismatch")
	}
	if _, err := os.Stat(dir + "/" + control.NodeCertFile); !os.IsNotExist(err) {
		t.Log("note: node TLS material is written by the daemon in production")
	}

	// Steps 5–7 in the store.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	metaE, err := res.Store.Get(ctx, store.Key(control.MetaKey))
	if err != nil {
		t.Fatalf("meta: %v", err)
	}
	var meta control.ClusterMeta
	if err := json.Unmarshal(metaE.Value, &meta); err != nil || meta.ID != res.ClusterID || meta.Name != "test" {
		t.Errorf("meta = %s", metaE.Value)
	}
	genE, _ := res.Store.Get(ctx, store.Key(control.GenerationKey))
	if string(genE.Value) != control.GenerationOne {
		t.Errorf("generation = %s, want 1", genE.Value)
	}
	nodes, _ := res.Store.List(ctx, store.Key(join.NodesKeyPrefix))
	if len(nodes) != 1 {
		t.Errorf("nodes = %d, want 1", len(nodes))
	}

	// Re-init is rejected.
	if _, err := control.Init(context.Background(), control.InitOptions{DataDir: dir}); !errors.Is(err, errors.KindConflict) {
		t.Errorf("re-init: err=%v, want conflict", err)
	}
}

// TestInitTokenJoinStatus is the card's acceptance flow: init → token →
// join ×2 → status shows 3 nodes, leader, quorum 3/2.
func TestInitTokenJoinStatus(t *testing.T) {
	r := newRig(t, "acceptance")

	n2 := r.enrollNode("n2", "voter")
	n3 := r.enrollNode("n3", "voter")

	// Joiners carry valid certs (signed by the cluster CA — verified
	// against it below).
	for _, res := range []*control.EnrollResult{n2, n3} {
		pool, _ := ca.NewBundle(r.initRes.CA.Cert)
		if _, err := res.Cert.Verify(x509.VerifyOptions{Roots: pool.Pool()}); err != nil {
			t.Errorf("%s cert does not verify: %v", res.NodeID, err)
		}
	}

	// Status from the leader's store.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rep, err := control.Status(ctx, r.initRes.Store)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(rep.Nodes) != 3 {
		t.Fatalf("nodes = %d, want 3", len(rep.Nodes))
	}
	if rep.Leader == "" || rep.Degraded {
		t.Errorf("leader=%q degraded=%v", rep.Leader, rep.Degraded)
	}
	if rep.QuorumNeed != 2 || rep.QuorumHave != 3 {
		t.Errorf("quorum %d/%d, want 3/2", rep.QuorumHave, rep.QuorumNeed)
	}
	if rep.Generation != 1 {
		t.Errorf("generation = %d, want 1", rep.Generation)
	}
	if rep.Name != "acceptance" || rep.ClusterID != r.initRes.ClusterID {
		t.Error("meta mismatch in report")
	}
	out := control.Render(rep)
	if want := "nodes:     3"; !strings.Contains(out, want) {
		t.Errorf("render missing %q:\n%s", want, out)
	}
	_ = pb.JoinPeer{} // peers surface in EnrollResult
}

func TestTokenListRevoke(t *testing.T) {
	r := newRig(t, "tokens")
	// The rig minted one token for the init result; mint two more.
	r.mintToken()
	r.mintToken()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	toks, err := control.ListTokens(ctx, r.initRes.Store)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if len(toks) < 3 {
		t.Fatalf("tokens = %d, want >= 3", len(toks))
	}
	for _, tok := range toks {
		if tok.Uses != 0 || tok.Max != 1 {
			t.Errorf("token %+v: uses/max wrong", tok)
		}
	}
	if err := control.RevokeToken(ctx, r.initRes.Store, r.initRes.Secret, toks[0].Nonce); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	after, _ := control.ListTokens(ctx, r.initRes.Store)
	if len(after) != len(toks)-1 {
		t.Fatalf("tokens after revoke = %d, want %d", len(after), len(toks)-1)
	}
}

func TestJoinersReplicateAndRead(t *testing.T) {
	r := newRig(t, "repl")
	n2 := r.enrollNode("n2", "voter")

	// Wait for the joiner to apply the log (the leader wrote meta
	// before n2 joined), then read locally (stale is fine in tests).
	deadline := time.Now().Add(10 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, err := n2.Store.Get(store.WithStale(ctx), store.Key(control.MetaKey))
		cancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("joiner never replicated meta: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// And the leader can read the joiner's node record (linearizable).
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	e, err := r.initRes.Store.Get(ctx, store.Key(join.NodesKeyPrefix+"n2"))
	if err != nil {
		t.Fatalf("node record: %v", err)
	}
	var rec join.NodeRecord
	if err := json.Unmarshal(e.Value, &rec); err != nil || rec.ID != "n2" {
		t.Errorf("record = %s", e.Value)
	}
	_ = raftstore.Config{}
}
