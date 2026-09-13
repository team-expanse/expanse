package control_test

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/cluster/ca"
	"github.com/expanse/expanse/internal/cluster/control"
	"github.com/expanse/expanse/internal/cluster/join"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/raftstore"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// TestInternalTLSConfigBuilders covers the :7443 mutual-TLS config
// builders against a live leader store with membership entries:
// a member's own cert satisfies the CN-membership check, a rogue cert
// (valid chain, unknown CN) must be rejected by the server-side config.
func TestInternalTLSConfigBuilders(t *testing.T) {
	r := newRig(t, "tls-builders")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Register the leader's node record so the CN-membership check has
	// an entry to match against.
	writeNodeRecord(r.t, r.initRes.Store, "n1")

	srvCfg, err := control.InternalTLS(ctx, r.initRes.Store, r.initRes.CA, r.dataDir)
	if err != nil {
		t.Fatalf("InternalTLS: %v", err)
	}
	cliCfg, err := control.InternalClientTLS(ctx, r.initRes.Store, r.initRes.CA, r.dataDir)
	if err != nil {
		t.Fatalf("InternalClientTLS: %v", err)
	}

	// Live server on a free port with the server-side config.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(srvCfg)))
	pb.RegisterInternalStoreServiceServer(srv, forwardServerFor(r))
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)

	// Member client: full handshake + a real forwarded RPC.
	if err := dialInternalOK(t, ln.Addr().String(), cliCfg); err != nil {
		t.Fatalf("member client rejected: %v", err)
	}

	// Rogue client: valid chain to the cluster CA, CN not a member.
	rogue := issueNodePair(r.t, r.dataDir, "rogue-node")
	rogueCfg := ca.PeerTLSConfig(mustBundle(r.t, r.dataDir), *rogue, func() []string { return []string{"n1"} })
	if err := dialInternalOK(t, ln.Addr().String(), rogueCfg); err == nil {
		t.Fatal("rogue CN accepted by internal endpoint (membership check not enforced)")
	}
}

// TestServeInternalEndpointOn drives the product wiring end to end:
// the endpoint loads its identity from the data dir, builds its own
// TLS config, hosts the forward server, and enforces CN membership.
func TestServeInternalEndpointOn(t *testing.T) {
	r := newRig(t, "internal-e2e")
	writeNodeRecord(r.t, r.initRes.Store, "n1")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- control.ServeInternalEndpointOn(ctx, r.initRes.Store, r.initRes.CA, r.dataDir, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(5 * time.Second):
		}
	})

	// Member client through the PRODUCT's client config builder.
	cliCtx, cliCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cliCancel()
	cliCfg, err := control.InternalClientTLS(cliCtx, r.initRes.Store, r.initRes.CA, r.dataDir)
	if err != nil {
		t.Fatalf("InternalClientTLS: %v", err)
	}
	if err := dialInternalOK(t, ln.Addr().String(), cliCfg); err != nil {
		t.Fatalf("forwarded RPC through ServeInternalEndpointOn failed: %v", err)
	}
}

// TestServeJoinEndpointOn wires the join service through the product
// function (identity from the data dir, TLS 1.3 server config) and runs
// a real Enroll against it.
func TestServeJoinEndpointOn(t *testing.T) {
	r := newRig(t, "join-e2e")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- control.ServeJoinEndpointOn(ctx, r.initRes.Store, r.initRes.CA, r.initRes.Secret, r.dataDir, r.initRes.ClusterID, ln)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(5 * time.Second):
		}
	})

	// Enroll against the product-served endpoint.
	joinerDir := t.TempDir()
	raftPort := freePort(t)
	res, err := control.Enroll(context.Background(), control.EnrollOptions{
		DataDir: joinerDir, NodeID: "n9", Address: ln.Addr().String(),
		Token: r.mintToken(), Role: "voter", APIAddr: "127.0.0.1:1",
		BindAddr:      fmt.Sprintf("127.0.0.1:%d", raftPort),
		AdvertiseAddr: fmt.Sprintf("127.0.0.1:%d", raftPort),
	})
	if err != nil {
		t.Fatalf("Enroll through ServeJoinEndpointOn: %v", err)
	}
	t.Cleanup(func() { _ = res.Store.Close() })
	if res.ClusterID != r.initRes.ClusterID {
		t.Errorf("joiner cluster id = %q, want %q", res.ClusterID, r.initRes.ClusterID)
	}
}

// --- helpers ---------------------------------------------------------------

// writeNodeRecord registers id in the replicated node table.
func writeNodeRecord(t *testing.T, st *raftstore.Store, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := st.Put(ctx, store.Key(join.NodesKeyPrefix+id), []byte(`{"role":"voter"}`)); err != nil {
		t.Fatalf("put node record %s: %v", id, err)
	}
}

// issueNodePair issues a fresh node cert/key for nodeID from the rig's
// persisted CA.
func issueNodePair(t *testing.T, dataDir, nodeID string) *tls.Certificate {
	t.Helper()
	_, _, clusterCA, err := control.LoadCluster(dataDir)
	if err != nil {
		t.Fatalf("LoadCluster: %v", err)
	}
	cert, priv, err := clusterCA.IssueNode(nodeID, "localhost", []net.IP{net.IPv4(127, 0, 0, 1)}, time.Now())
	if err != nil {
		t.Fatalf("IssueNode %s: %v", nodeID, err)
	}
	return &tls.Certificate{Certificate: [][]byte{cert.Raw, clusterCA.Cert.Raw}, PrivateKey: priv}
}

func mustBundle(t *testing.T, dataDir string) *ca.Bundle {
	t.Helper()
	_, _, clusterCA, err := control.LoadCluster(dataDir)
	if err != nil {
		t.Fatalf("LoadCluster: %v", err)
	}
	b, err := ca.NewBundle(clusterCA.Cert)
	if err != nil {
		t.Fatalf("NewBundle: %v", err)
	}
	return b
}

// forwardServerFor returns the internal forward server for the rig.
func forwardServerFor(r *rig) pb.InternalStoreServiceServer {
	return raftstore.NewForwardServer(r.initRes.Store)
}

// dialInternalOK dials addr with cfg, issues one forwarded write + read,
// and returns nil when both succeed.
func dialInternalOK(t *testing.T, addr string, cfg *tls.Config) error {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cli := pb.NewInternalStoreServiceClient(conn)
	cmd := &pb.Command{Type: pb.CommandType_COMMAND_TYPE_PUT, Key: "dial-check", Value: []byte("ok"), TimestampUnixNs: time.Now().UnixNano()}
	if _, err := cli.ForwardCommand(ctx, cmd); err != nil {
		return fmt.Errorf("ForwardCommand: %w", err)
	}
	resp, err := cli.LinearRead(ctx, &pb.LinearReadRequest{Query: &pb.LinearReadRequest_GetKey{GetKey: "dial-check"}})
	if err != nil {
		return fmt.Errorf("LinearRead: %w", err)
	}
	if resp.GetEntry() == nil {
		return fmt.Errorf("LinearRead returned no entry")
	}
	return nil
}

// TestWaitForLeader covers both branches of the readiness poll.
func TestWaitForLeader(t *testing.T) {
	r := newRig(t, "wait-leader")
	ctx := context.Background()
	if !control.WaitForLeader(ctx, r.initRes.Store, 30*time.Second) {
		t.Fatal("WaitForLeader timed out on a healthy leader")
	}
	// An immediate deadline with a known leader still succeeds: the
	// leader check precedes the deadline check.
	if !control.WaitForLeader(ctx, r.initRes.Store, 0) {
		t.Error("WaitForLeader(0 timeout, leader present) = false")
	}
}

// TestServeJoinEndpointMissingIdentity covers the wrapper's identity
// load failure path (no node cert in the data dir).
func TestServeJoinEndpointMissingIdentity(t *testing.T) {
	r := newRig(t, "join-no-identity")
	emptyDir := t.TempDir()
	err := control.ServeJoinEndpoint(context.Background(), r.initRes.Store, r.initRes.CA,
		r.initRes.Secret, emptyDir, r.initRes.ClusterID)
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("err = %v, want missing cert", err)
	}
}

// TestStatusReport covers the cluster-status aggregation against the
// rig (leader, voters, witness counting, degraded flag).
func TestStatusReport(t *testing.T) {
	r := newRig(t, "status-report")
	r.enrollNode("n2", "voter")
	r.enrollNode("nw", "witness")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rep, err := control.Status(ctx, r.initRes.Store)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if rep.ClusterID == "" || rep.Name != "status-report" {
		t.Errorf("meta = %+v", rep)
	}
	if rep.Leader == "" || rep.Degraded {
		t.Errorf("leader view: leader=%q degraded=%v", rep.Leader, rep.Degraded)
	}
	if rep.QuorumHave < 3 { // n1 + n2 + witness all count (§4.9)
		t.Errorf("quorum-have = %d, want >= 3", rep.QuorumHave)
	}
	if rep.Generation < 1 {
		t.Errorf("generation = %d", rep.Generation)
	}
}

// TestServeInternalEndpointMissingIdentity covers the wrapper's
// identity-load failure path.
func TestServeInternalEndpointMissingIdentity(t *testing.T) {
	r := newRig(t, "internal-no-identity")
	emptyDir := t.TempDir()
	err := control.ServeInternalEndpoint(context.Background(), r.initRes.Store, r.initRes.CA, emptyDir)
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("err = %v, want missing cert", err)
	}
}
