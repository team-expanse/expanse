package service

import (
	"context"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/blocks/validate"
	"github.com/expanse/expanse/internal/store/raftstore"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// fakeCatalog satisfies validate.Catalog for admission tests.
type fakeCatalog struct{}

func (fakeCatalog) HasType(t string) bool { return t == "util/echo" }
func (fakeCatalog) Types() []string       { return []string{"util/echo"} }
func (fakeCatalog) ValidateConfig(t string, c *structpb.Struct) []string {
	return nil
}

func newServer(t *testing.T) (*Server, func(context.Context) (uint64, error)) {
	t.Helper()
	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ln.Close()
	st, err := raftstore.Open(raftstore.Config{
		NodeID:    "n1",
		BindAddr:  ln.Addr().String(),
		DataDir:   dir,
		Bootstrap: true,
		LogOutput: io.Discard,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && st.Leader() == "" {
		time.Sleep(20 * time.Millisecond)
	}
	if st.Leader() == "" {
		t.Fatal("no leader elected")
	}
	srv := New(st, func() validate.Context {
		return validate.Context{
			Catalog:   fakeCatalog{},
			NodeCount: 3,
		}
	})
	return srv, func(ctx context.Context) (uint64, error) {
		r, err := st.Revision(ctx)
		return uint64(r), err
	}
}

func validBlock(name string) *pb.Block {
	i := int32(1)
	return &pb.Block{
		Metadata: &pb.Metadata{Name: name, Namespace: "default"},
		Spec: &pb.BlockSpec{
			Type:     "util/echo",
			Replicas: &i,
			Resources: &pb.Resources{Requests: &pb.ResourcePair{
				Cpu: "100m", Memory: "64Mi",
			}},
		},
	}
}

func gen(t *testing.T, ctx context.Context, s *Server) uint64 {
	t.Helper()
	g, err := generationCurrent(ctx, s.St)
	if err != nil {
		t.Fatalf("generationCurrent: %v", err)
	}
	return g
}

func TestCreatePersistsAndBumpsGenerationOnce(t *testing.T) {
	ctx := context.Background()
	s, _ := newServer(t)
	g0 := gen(t, ctx, s)

	created, err := s.Create(ctx, validBlock("web"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.GetStatus().GetPhase() != pb.Phase_PENDING {
		t.Errorf("phase = %v, want PENDING", created.GetStatus().GetPhase())
	}
	if created.GetStatus().GetObservedGeneration() != int64(g0+1) {
		t.Errorf("observedGeneration = %d, want %d",
			created.GetStatus().GetObservedGeneration(), g0+1)
	}

	got, err := s.Get(ctx, &pb.GetBlockRequest{Namespace: "default", Name: "web"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.GetSpec().GetType() != "util/echo" {
		t.Errorf("spec type = %q", got.GetSpec().GetType())
	}
	if got.GetStatus().GetPhase() != pb.Phase_PENDING {
		t.Errorf("persisted phase = %v", got.GetStatus().GetPhase())
	}

	if g := gen(t, ctx, s); g != g0+1 {
		t.Errorf("generation = %d, want exactly %d (bumped once)", g, g0+1)
	}
}

func TestInvalidCreateZeroWritesZeroGenBump(t *testing.T) {
	ctx := context.Background()
	s, revAt := newServer(t)
	g0 := gen(t, ctx, s)
	r0, err := revAt(ctx)
	if err != nil {
		t.Fatalf("revision: %v", err)
	}

	bad := validBlock("bad")
	bad.Spec.Type = "nope/missing" // V3: unknown type
	if _, err := s.Create(ctx, bad); err == nil {
		t.Fatal("Create accepted an unknown type")
	}

	r1, err := revAt(ctx)
	if err != nil {
		t.Fatalf("revision: %v", err)
	}
	if r1 != r0 {
		t.Errorf("store revision moved %d -> %d on rejected create", r0, r1)
	}
	if g := gen(t, ctx, s); g != g0 {
		t.Errorf("generation bumped to %d on rejected create, want %d", g, g0)
	}
	lst, err := s.List(ctx, &pb.ListBlocksRequest{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(lst.GetBlocks()) != 0 {
		t.Errorf("rejected create left %d blocks", len(lst.GetBlocks()))
	}
}

func TestGetListDeleteRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, _ := newServer(t)

	for _, n := range []string{"a", "b"} {
		if _, err := s.Create(ctx, validBlock(n)); err != nil {
			t.Fatalf("Create %s: %v", n, err)
		}
	}
	if _, err := s.Create(ctx, func() *pb.Block {
		b := validBlock("other-ns")
		b.Metadata.Namespace = "prod"
		return b
	}()); err != nil {
		t.Fatalf("Create prod: %v", err)
	}

	lst, err := s.List(ctx, &pb.ListBlocksRequest{Namespace: "default"})
	if err != nil {
		t.Fatalf("List default: %v", err)
	}
	var names []string
	for _, b := range lst.GetBlocks() {
		names = append(names, b.GetMetadata().GetName())
	}
	if fmt.Sprint(names) != "[a b]" {
		t.Errorf("namespaced list = %v, want [a b]", names)
	}
	all, err := s.List(ctx, &pb.ListBlocksRequest{})
	if err != nil || len(all.GetBlocks()) != 3 {
		t.Fatalf("full list = %d blocks (err %v), want 3", len(all.GetBlocks()), err)
	}

	// Delete round-trips the deleted block back.
	del, err := s.Delete(ctx, &pb.DeleteBlockRequest{Namespace: "default", Name: "a"})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if del.GetMetadata().GetName() != "a" {
		t.Errorf("deleted block = %+v", del.GetMetadata())
	}
	if _, err := s.Get(ctx, &pb.GetBlockRequest{Namespace: "default", Name: "a"}); err == nil {
		t.Error("Get after Delete succeeded")
	}
}

func TestUpdateBumpsGenerationAndKeepsStatus(t *testing.T) {
	ctx := context.Background()
	s, _ := newServer(t)
	created, err := s.Create(ctx, validBlock("web"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	g0 := gen(t, ctx, s)

	upd := proto.Clone(created).(*pb.Block)
	upd.Metadata.Labels = map[string]string{"tier": "web"}
	upd.Spec.Resources.Requests.Cpu = "250m"
	out, err := s.Update(ctx, upd)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if out.GetStatus().GetPhase() != pb.Phase_PENDING {
		t.Errorf("status phase changed on update: %v", out.GetStatus().GetPhase())
	}

	got, err := s.Get(ctx, &pb.GetBlockRequest{Namespace: "default", Name: "web"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.GetSpec().GetResources().GetRequests().GetCpu() != "250m" {
		t.Errorf("update not persisted: %v", got.GetSpec().GetResources())
	}
	if got.GetMetadata().GetLabels()["tier"] != "web" {
		t.Errorf("labels not persisted: %v", got.GetMetadata().GetLabels())
	}
	if g := gen(t, ctx, s); g != g0+1 {
		t.Errorf("generation = %d, want %d after update", g, g0+1)
	}
}

func TestUpdateMissingBlockFails(t *testing.T) {
	ctx := context.Background()
	s, _ := newServer(t)
	if _, err := s.Update(ctx, validBlock("ghost")); err == nil {
		t.Error("Update of missing block succeeded")
	}
}

func TestDuplicateNameRejectedV2(t *testing.T) {
	ctx := context.Background()
	s, _ := newServer(t)
	if _, err := s.Create(ctx, validBlock("web")); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	g0 := gen(t, ctx, s)
	if _, err := s.Create(ctx, validBlock("web")); err == nil {
		t.Fatal("duplicate Create succeeded (V2)")
	}
	if g := gen(t, ctx, s); g != g0 {
		t.Errorf("generation bumped on duplicate: %d", g)
	}
	// Same name in a DIFFERENT namespace is fine.
	if _, err := s.Create(ctx, func() *pb.Block {
		b := validBlock("web")
		b.Metadata.Namespace = "prod"
		return b
	}()); err != nil {
		t.Fatalf("cross-namespace same name: %v", err)
	}
}
