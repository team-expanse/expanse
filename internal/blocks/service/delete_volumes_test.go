package service

import (
	"context"
	"testing"

	"github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
)

// putStoredBlock writes b with one storage entry, plus volumes named for it and for another block.
func putStoredBlock(t *testing.T, s *Server) {
	t.Helper()
	ctx := context.Background()
	b := validBlock("web")
	b.Spec.Storage = []*pb.Storage{{Name: "data", Size: "1Gi"}}
	raw, err := proto.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.St.Put(ctx, key("default", "web"), raw); err != nil {
		t.Fatal(err)
	}
	for id, name := range map[string]string{
		"vol-1": "blk-default-web-data", "vol-2": "blk-default-web-data-1", "vol-3": "blk-default-webx-data",
	} {
		if err := storage.SaveSpec(ctx, s.St, storage.Spec{ID: id, Name: name, SizeBytes: 1 << 30, Replication: 1}); err != nil {
			t.Fatal(err)
		}
	}
}

func deleteOps(t *testing.T, s *Server) map[string]bool {
	t.Helper()
	es, err := s.St.List(context.Background(), store.Key("/volumes/_ops/delete/"))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range es {
		got[string(e.Key)] = true
	}
	return got
}

func TestDeleteKeepsTheBlocksVolumesByDefault(t *testing.T) {
	s, _ := newServer(t)
	putStoredBlock(t, s)
	if _, err := s.Delete(context.Background(), &pb.DeleteBlockRequest{Namespace: "default", Name: "web"}); err != nil {
		t.Fatal(err)
	}
	if ops := deleteOps(t, s); len(ops) != 0 {
		t.Errorf("delete ops = %v, want none", ops)
	}
}

func TestDeleteWithVolumesQueuesTheBlocksVolumesOnly(t *testing.T) {
	s, _ := newServer(t)
	putStoredBlock(t, s)
	if _, err := s.Delete(context.Background(), &pb.DeleteBlockRequest{Namespace: "default", Name: "web", DeleteVolumes: true}); err != nil {
		t.Fatal(err)
	}
	ops := deleteOps(t, s)
	if !ops["/volumes/_ops/delete/vol-1"] || !ops["/volumes/_ops/delete/vol-2"] || ops["/volumes/_ops/delete/vol-3"] || len(ops) != 2 {
		t.Errorf("delete ops = %v, want vol-1 and vol-2", ops)
	}
}
