package service

// Replica health on read: each placement carries its own node's latest probe records.

import (
	"context"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/blocks/health"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
)

// placeOn stores an observed status placing replicas 0 and 1 of default/web on n1 and n2.
func placeOn(t *testing.T, ctx context.Context, s *Server) {
	t.Helper()
	if _, err := s.Create(ctx, validBlock("web")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	raw, _ := proto.Marshal(&pb.BlockStatus{Placements: []*pb.PlacementStatus{
		{ReplicaIndex: 0, NodeId: "n1", Phase: pb.Phase_RUNNING},
		{ReplicaIndex: 1, NodeId: "n2", Phase: pb.Phase_RUNNING},
	}})
	if _, err := s.St.Put(ctx, key("default", "web")+"/status", raw); err != nil {
		t.Fatal(err)
	}
}

func TestGetFillsEachReplicasHealthFromItsNode(t *testing.T) {
	ctx := context.Background()
	s, _ := newServer(t)
	placeOn(t, ctx, s)
	at := time.Unix(1700000000, 0)
	if err := health.StorePublisher(s.St, key("default", "web"), 0, "n1")(ctx, health.Record{OK: true, Detail: "200 OK", At: at}); err != nil {
		t.Fatal(err)
	}
	if err := health.LivenessPublisher(s.St, key("default", "web"), 0, "n1")(ctx, health.LivenessRecord{Restarts: 2, Detail: "refused", At: at}); err != nil {
		t.Fatal(err)
	}
	if err := health.StorePublisher(s.St, key("default", "web"), 1, "elsewhere")(ctx, health.Record{OK: true}); err != nil {
		t.Fatal(err)
	}

	b, err := s.Get(ctx, &pb.GetBlockRequest{Namespace: "default", Name: "web"})
	if err != nil {
		t.Fatal(err)
	}
	h := b.GetStatus().GetPlacements()[0].GetHealth()
	if r := h.GetReadiness(); !r.GetOk() || r.GetDetail() != "200 OK" || r.GetAtUnixNs() != at.UnixNano() {
		t.Errorf("replica 0 readiness = %+v", r)
	}
	if l := h.GetLiveness(); !l.GetOk() || l.GetDetail() != "refused" || h.GetRestarts() != 2 {
		t.Errorf("replica 0 liveness = %+v, restarts %d; want ok (still restarting), refused, 2", l, h.GetRestarts())
	}
	if h := b.GetStatus().GetPlacements()[1].GetHealth(); h.GetReadiness() != nil || h.GetLiveness() != nil {
		t.Errorf("replica 1 shows another node's record: %+v", h)
	}

	lst, err := s.List(ctx, &pb.ListBlocksRequest{})
	if err != nil || lst.GetBlocks()[0].GetStatus().GetPlacements()[0].GetHealth().GetReadiness() == nil {
		t.Errorf("List does not carry health (err %v)", err)
	}
}

func TestGivenUpReplicaShowsLivenessNotOk(t *testing.T) {
	ctx := context.Background()
	s, _ := newServer(t)
	placeOn(t, ctx, s)
	if err := health.LivenessPublisher(s.St, key("default", "web"), 1, "n2")(ctx, health.LivenessRecord{Restarts: 5, Failed: true}); err != nil {
		t.Fatal(err)
	}
	b, _ := s.Get(ctx, &pb.GetBlockRequest{Namespace: "default", Name: "web"})
	if l := b.GetStatus().GetPlacements()[1].GetHealth().GetLiveness(); l == nil || l.GetOk() {
		t.Errorf("replica 1 liveness = %+v, want not ok", l)
	}
}

func TestWatchDeliversProbeRecordChanges(t *testing.T) {
	ctx := context.Background()
	s, _ := newServer(t)
	placeOn(t, ctx, s)
	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	recv := make(chan *pb.BlockEvent, 4)
	go func() {
		_ = s.Watch(&pb.WatchBlocksRequest{Namespace: "default", Name: "web"}, &fakeWatchServer{ctx: watchCtx, recv: recv})
	}()
	time.Sleep(50 * time.Millisecond) // let Watch start listening

	if err := health.StorePublisher(s.St, key("default", "web"), 0, "n1")(ctx, health.Record{Detail: "503"}); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-recv:
		if d := ev.GetBlock().GetStatus().GetPlacements()[0].GetHealth().GetReadiness().GetDetail(); d != "503" {
			t.Errorf("event readiness detail = %q, want 503", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Watch never delivered the probe record change")
	}
}

func TestReadyCountsRunningReplicasNotFailingReadiness(t *testing.T) {
	ctx := context.Background()
	s, _ := newServer(t)
	placeOn(t, ctx, s)
	b, _ := s.Get(ctx, &pb.GetBlockRequest{Namespace: "default", Name: "web"})
	if n := b.GetStatus().GetReplicas().GetReady(); n != 2 {
		t.Errorf("ready = %d with no probe records, want 2", n)
	}
	if err := health.StorePublisher(s.St, key("default", "web"), 1, "n2")(ctx, health.Record{OK: false}); err != nil {
		t.Fatal(err)
	}
	b, _ = s.Get(ctx, &pb.GetBlockRequest{Namespace: "default", Name: "web"})
	if n := b.GetStatus().GetReplicas().GetReady(); n != 1 {
		t.Errorf("ready = %d with replica 1 failing readiness, want 1", n)
	}
}
