package api

// T20.5a: the block API (BlockService + CatalogService) is served on the
// agent socket when configured.
import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/expanse/expanse/internal/blocks/catalog"
	"github.com/expanse/expanse/internal/blocks/service"
	"github.com/expanse/expanse/internal/blocks/validate"
	"github.com/expanse/expanse/internal/blocks/wire"
	"github.com/expanse/expanse/internal/store/raftstore"
	pb "github.com/expanse/expanse/proto"
)

func TestBlockAPIServedOnAgentSocket(t *testing.T) {
	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	st, err := raftstore.Open(raftstore.Config{
		NodeID: "n1", BindAddr: ln.Addr().String(), DataDir: dir,
		Bootstrap: true, LogOutput: io.Discard,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for i := 0; i < 500 && st.Leader() == ""; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if st.Leader() == "" {
		t.Fatal("no leader")
	}

	cat, err := catalog.Load("../../nix/blocks")
	if err != nil {
		t.Fatalf("Load catalog: %v", err)
	}
	blk := service.New(st, func() validate.Context {
		return validate.Context{Catalog: cat, NodeCount: 3}
	})
	blk.Nodes = wire.Nodes(st)

	socket := filepath.Join(t.TempDir(), "agent.sock")
	srv := NewServer(nil /* agent: unused by block RPCs */, st,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv.Blocks = blk
	srv.Catalog = service.NewCatalogServer(cat)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Serve(ctx, socket) }()

	// Wait for the socket.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	conn, err := grpc.NewClient("unix://"+socket,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// Create a block through the real gRPC surface.
	c := pb.NewBlockServiceClient(conn)
	two := int32(2)
	cfg, err := structpb.NewStruct(map[string]any{"port": 18080})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Create(ctx, &pb.Block{
		Metadata: &pb.Metadata{Name: "web", Namespace: "default"},
		Spec: &pb.BlockSpec{
			Type:     "util/echo",
			Replicas: &two,
			Config:   cfg,
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	list, err := c.List(ctx, &pb.ListBlocksRequest{Namespace: "default"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list.GetBlocks()) != 1 {
		t.Fatalf("List = %d blocks, want 1", len(list.GetBlocks()))
	}

	// Catalog RPC serves the shipped types.
	cc := pb.NewCatalogServiceClient(conn)
	types, err := cc.ListTypes(ctx, &pb.ListTypesRequest{})
	if err != nil {
		t.Fatalf("ListTypes: %v", err)
	}
	found := false
	for _, typ := range types.GetTypes() {
		if typ.GetName() == "util/echo" {
			found = true
		}
	}
	if !found {
		t.Errorf("util/echo missing from ListTypes: %v", types.GetTypes())
	}

	// Explain runs through the wired Nodes adapter: both replicas
	// pending, each explained against the one node record.
	exCtx, exCancel := context.WithTimeout(ctx, 10*time.Second)
	defer exCancel()
	ex, err := c.Explain(exCtx, &pb.ExplainRequest{Namespace: "default", Name: "web"})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(ex.GetReplicas()) != 2 {
		t.Fatalf("Explain replicas = %d, want 2", len(ex.GetReplicas()))
	}
}
