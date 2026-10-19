package service

import (
	"context"
	"strings"
	"testing"

	"github.com/expanse/expanse/internal/quantity"
	"github.com/expanse/expanse/internal/scheduler"
	pb "github.com/expanse/expanse/proto"
)

func preflightServer(t *testing.T, nodes []scheduler.NodeView) *Server {
	t.Helper()
	srv, _ := newServer(t)
	srv.Nodes = func(context.Context) ([]scheduler.NodeView, scheduler.OvercommitConfig, error) {
		return nodes, schedCfg(), nil
	}
	return srv
}

func TestPreflightPlacesEachReplicaWithoutStoringAnything(t *testing.T) {
	ctx := context.Background()
	srv := preflightServer(t, expNodes())
	b := validBlock("web")
	b.Spec.Replicas = i32(2)
	b.Spec.Placement = &pb.Placement{AntiAffinity: pb.AntiAffinity_ANTI_AFFINITY_NODE}

	plan, err := srv.Preflight(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 2 || plan[0].Node == "" || plan[1].Node == "" || plan[0].Node == plan[1].Node {
		t.Fatalf("plan = %+v, want two replicas on two nodes", plan)
	}
	if _, err := srv.Get(ctx, &pb.GetBlockRequest{Namespace: "default", Name: "web"}); err == nil {
		t.Error("the pre-flight stored the block")
	}
}

func TestPreflightSaysWhyAReplicaDoesNotFit(t *testing.T) {
	srv := preflightServer(t, expNodes())
	b := validBlock("web")
	b.Spec.Replicas = i32(3)
	b.Spec.Placement = &pb.Placement{AntiAffinity: pb.AntiAffinity_ANTI_AFFINITY_NODE}

	plan, err := srv.Preflight(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	last := plan[2]
	if last.Node != "" || !strings.HasPrefix(last.Rejected["n1"], scheduler.CodeAntiAffinity) ||
		!strings.HasPrefix(last.Rejected["n3"], scheduler.CodeNotReady) {
		t.Fatalf("replica 2 = %+v", last)
	}
}

func TestPreflightCountsCapacityAcrossReplicas(t *testing.T) {
	srv := preflightServer(t, expNodes()[:1])
	b := validBlock("big")
	b.Spec.Replicas = i32(2)
	b.Spec.Resources.Requests.Memory = "5Gi" // n1 has 8Gi: room for one

	plan, err := srv.Preflight(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	if plan[0].Node != "n1" || !strings.HasPrefix(plan[1].Rejected["n1"], scheduler.CodeInsufficientMemory) {
		t.Fatalf("plan = %+v", plan)
	}
	if plan[1].Code != scheduler.CodeInsufficientMemory || plan[1].Suggestion == "" {
		t.Error("no suggestion for a uniform rejection")
	}
}

func TestPreflightSingletonWithStorageFitsBeforeItsVolumeExists(t *testing.T) {
	srv := preflightServer(t, expNodes())
	b := validBlock("db")
	b.Spec.Replicas = nil
	b.Spec.Strategy = &pb.Strategy{Kind: pb.StrategyKind_SINGLETON}
	b.Spec.Storage = []*pb.Storage{{Name: "data", Size: "10Gi"}}

	plan, err := srv.Preflight(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 1 || plan[0].Node == "" {
		t.Fatalf("plan = %+v, want the singleton placed", plan)
	}
}

func TestPreflightDaemonsetListsEveryNode(t *testing.T) {
	nodes := expNodes()
	nodes[1].FreeMem = quantity.Bytes{N: 1 << 20}
	srv := preflightServer(t, nodes)
	b := validBlock("agent")
	b.Spec.Replicas = nil
	b.Spec.Strategy = &pb.Strategy{Kind: pb.StrategyKind_DAEMONSET}

	plan, err := srv.Preflight(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 1 || plan[0].Node != "n1" {
		t.Fatalf("plan = %+v, want one replica on n1", plan)
	}
}

func TestPreflightWithNoNodes(t *testing.T) {
	srv := preflightServer(t, nil)
	plan, err := srv.Preflight(context.Background(), validBlock("web"))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 1 || plan[0].Node != "" || plan[0].Suggestion == "" {
		t.Fatalf("plan = %+v", plan)
	}
}
