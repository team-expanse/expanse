package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/expanse/expanse/internal/cluster/join"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/raftstore"
	pb "github.com/expanse/expanse/proto"
)

// serveNodeAPI runs a one-node cluster agent API with node records for ids.
func serveNodeAPI(t *testing.T, ids ...string) pb.NodeServiceClient {
	t.Helper()
	st, err := raftstore.Open(raftstore.Config{
		NodeID: "n1", BindAddr: "127.0.0.1:0", DataDir: t.TempDir(), Bootstrap: true, LogOutput: io.Discard,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for i := 0; i < 500 && !st.IsLeader(); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	for _, id := range ids {
		b, _ := json.Marshal(join.NodeRecord{ID: id, Role: "voter", JoinedAt: time.Now().UnixNano()})
		if _, err := st.Put(ctx, store.Key(join.NodesKeyPrefix+id), b); err != nil {
			t.Fatalf("write node %s: %v", id, err)
		}
	}

	socket := filepath.Join(t.TempDir(), "agent.sock")
	srv := NewServer(nil, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	go func() { _ = srv.Serve(ctx, socket) }()
	for i := 0; i < 500; i++ {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	conn, err := grpc.NewClient("unix://"+socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return pb.NewNodeServiceClient(conn)
}

func cordoned(t *testing.T, c pb.NodeServiceClient, id string) bool {
	t.Helper()
	res, err := c.ListNodes(context.Background(), &pb.ListNodesRequest{})
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	for _, n := range res.GetNodes() {
		if n.GetId() == id {
			return n.GetCordoned()
		}
	}
	t.Fatalf("node %s not listed: %v", id, res.GetNodes())
	return false
}

func TestCordonAndUncordonThroughTheRunningAgent(t *testing.T) {
	c := serveNodeAPI(t, "n1", "n2")
	ctx := context.Background()

	if _, err := c.SetNodeCordon(ctx, &pb.SetNodeCordonRequest{NodeId: "n2", Cordoned: true}); err != nil {
		t.Fatalf("cordon: %v", err)
	}
	if !cordoned(t, c, "n2") {
		t.Error("n2 not cordoned")
	}
	if _, err := c.SetNodeCordon(ctx, &pb.SetNodeCordonRequest{NodeId: "n2"}); err != nil {
		t.Fatalf("uncordon: %v", err)
	}
	if cordoned(t, c, "n2") {
		t.Error("n2 still cordoned")
	}
	_, err := c.SetNodeCordon(ctx, &pb.SetNodeCordonRequest{NodeId: "ghost", Cordoned: true})
	if status.Code(err) != codes.NotFound {
		t.Errorf("cordon ghost = %v, want NotFound", err)
	}
}

func TestDrainThroughTheRunningAgentCordonsAndCounts(t *testing.T) {
	c := serveNodeAPI(t, "n1", "n2")
	res, err := c.DrainNode(context.Background(), &pb.DrainNodeRequest{NodeId: "n2"})
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if res.GetResources() != 0 || !cordoned(t, c, "n2") {
		t.Errorf("drain = %d resources, cordoned=%v; want 0, true", res.GetResources(), cordoned(t, c, "n2"))
	}
}

func TestRemoveThroughTheRunningAgentKeepsTheInterlocks(t *testing.T) {
	c := serveNodeAPI(t, "n1", "n2")
	_, err := c.RemoveNode(context.Background(), &pb.RemoveNodeRequest{NodeId: "n2"})
	if status.Code(err) != codes.Aborted {
		t.Errorf("quorum-breaking remove = %v, want Aborted", err)
	}
}
