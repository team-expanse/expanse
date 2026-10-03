package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"

	"github.com/expanse/expanse/internal/cluster/control"
	pb "github.com/expanse/expanse/proto"
)

// fakeAgent records the cluster-admin requests the CLI sends.
type fakeAgent struct {
	pb.UnimplementedNodeServiceServer
	token    *pb.CreateJoinTokenRequest
	remove   *pb.RemoveNodeRequest
	transfer *pb.TransferLeadershipRequest
}

func (f *fakeAgent) CreateJoinToken(_ context.Context, r *pb.CreateJoinTokenRequest) (*pb.CreateJoinTokenResponse, error) {
	f.token = r
	return &pb.CreateJoinTokenResponse{Token: "expanse-join-fake"}, nil
}

func (f *fakeAgent) GetCARotation(context.Context, *pb.GetCARotationRequest) (*pb.GetCARotationResponse, error) {
	return &pb.GetCARotationResponse{Rotating: true, Fingerprint: "ab:cd", Pending: []string{"n2", "n3"}}, nil
}

func (f *fakeAgent) RemoveNode(_ context.Context, r *pb.RemoveNodeRequest) (*pb.RemoveNodeResponse, error) {
	f.remove = r
	return &pb.RemoveNodeResponse{}, nil
}

func (f *fakeAgent) TransferLeadership(_ context.Context, r *pb.TransferLeadershipRequest) (*pb.TransferLeadershipResponse, error) {
	f.transfer = r
	return &pb.TransferLeadershipResponse{Leader: r.GetTo()}, nil
}

// serveFakeAgent serves f on a unix socket and returns its path.
func serveFakeAgent(t *testing.T, f *fakeAgent) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "agent.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	pb.RegisterNodeServiceServer(srv, f)
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(srv.Stop)
	return sock
}

func run(t *testing.T, cmd *cobra.Command, args ...string) string {
	t.Helper()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out.String())
	}
	return out.String()
}

func TestTokenCreateUsesTheRunningAgent(t *testing.T) {
	f := &fakeAgent{}
	sock := serveFakeAgent(t, f)
	out := run(t, newClusterTokenCmd(), "create", "--socket", sock, "--uses", "3", "--ttl", "1h", "--for-node", "n2")
	if strings.TrimSpace(out) != "expanse-join-fake" {
		t.Errorf("output = %q, want the token alone", out)
	}
	if f.token.GetUses() != 3 || f.token.GetTtlSeconds() != 3600 || f.token.GetForNode() != "n2" {
		t.Errorf("request = %v", f.token)
	}
}

func TestCAStatusListsPendingNodes(t *testing.T) {
	sock := serveFakeAgent(t, &fakeAgent{})
	out := run(t, newClusterCACmd(), "status", "--socket", sock)
	if !strings.Contains(out, "ab:cd") || !strings.Contains(out, "n2") || !strings.Contains(out, "n3") {
		t.Errorf("output = %q, want fingerprint and pending nodes", out)
	}
}

func TestLeaveRemovesThisNodeByDefault(t *testing.T) {
	f := &fakeAgent{}
	sock := serveFakeAgent(t, f)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, control.NodeIDFile), []byte("n7"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, newClusterLeaveCmd(), "--socket", sock, "--data-dir", dir)
	if f.remove.GetNodeId() != "n7" {
		t.Errorf("removed %q, want this node (n7)", f.remove.GetNodeId())
	}
}

func TestTransferLeadershipNamesTheNewLeader(t *testing.T) {
	f := &fakeAgent{}
	sock := serveFakeAgent(t, f)
	out := run(t, newCtlCmd(), "node", "transfer-leadership", "n2", "--socket", sock)
	if f.transfer.GetTo() != "n2" || !strings.Contains(out, "n2") {
		t.Errorf("request %v, output %q; want transfer to n2 reported", f.transfer, out)
	}
}

func TestTokenCreateWithoutAnAgentOpensTheStore(t *testing.T) {
	dir := t.TempDir()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	raftAddr := l.Addr().String()
	_ = l.Close()
	res, err := control.Init(context.Background(), control.InitOptions{
		DataDir: dir, NodeID: "n1", Name: "offline", AdvertiseAddr: raftAddr, BindAddr: raftAddr, JoinHost: "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	_ = res.Store.Close()

	noAgent := filepath.Join(t.TempDir(), "agent.sock")
	out := run(t, newClusterTokenCmd(), "create", "--socket", noAgent, "--data-dir", dir)
	if !strings.HasPrefix(strings.TrimSpace(out), "expanse-join-") {
		t.Errorf("output = %q, want a join token", out)
	}
}
