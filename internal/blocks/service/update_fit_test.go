package service

// An update whose larger requests don't fit a placement's node is refused: replicas update in place.

import (
	"context"
	"strings"
	"testing"

	"github.com/expanse/expanse/internal/cluster/join"
	"github.com/expanse/expanse/internal/quantity"
	"github.com/expanse/expanse/internal/scheduler"
	pb "github.com/expanse/expanse/proto"
)

// withNodes gives s a cluster view of n1 and n2, each with 1 cpu and freeMem free beyond web's replicas.
func withNodes(s *Server, freeMem string) {
	mem, _ := quantity.ParseBytes(freeMem)
	s.Nodes = func(context.Context) ([]scheduler.NodeView, scheduler.OvercommitConfig, error) {
		var nodes []scheduler.NodeView
		for _, id := range []string{"n1", "n2"} {
			nodes = append(nodes, scheduler.NodeView{ID: id, Ready: true, FreeCPU: quantity.CPU{Milli: 1000}, FreeMem: mem})
		}
		return nodes, scheduler.OvercommitConfig{CPUOvercommitRatio: 1, MemoryOvercommitRatio: 1}, nil
	}
}

func webWithMemory(mem string) *pb.Block {
	b := validBlock("web")
	b.Spec.Resources.Requests.Memory = mem
	return b
}

func TestAnUpdateThatOutgrowsAPlacementsNodeIsRefused(t *testing.T) {
	ctx := context.Background()
	s, _ := newServer(t)
	placeOn(t, ctx, s)
	withNodes(s, "128Mi")
	_, err := s.Update(ctx, webWithMemory("1Gi"))
	if err == nil || !strings.Contains(err.Error(), scheduler.CodeInsufficientMemory) || !strings.Contains(err.Error(), "n1") {
		t.Fatalf("growth by 960Mi on nodes with 128Mi free: err %v", err)
	}
	if got, _ := s.get(ctx, "default", "web"); got.GetSpec().GetResources().GetRequests().GetMemory() != "64Mi" {
		t.Errorf("a refused update was stored: %v", got.GetSpec().GetResources())
	}
}

func TestAnUpdateThatFitsItsPlacementsNodesIsStored(t *testing.T) {
	ctx := context.Background()
	s, _ := newServer(t)
	placeOn(t, ctx, s)
	withNodes(s, "128Mi")
	if _, err := s.Update(ctx, webWithMemory("192Mi")); err != nil {
		t.Fatalf("growth by 128Mi on nodes with 128Mi free: %v", err)
	}
}

func TestAnUnplacedBlockGrowsFreely(t *testing.T) {
	ctx := context.Background()
	s, _ := newServer(t)
	if _, err := s.Create(ctx, validBlock("web")); err != nil {
		t.Fatal(err)
	}
	withNodes(s, "0")
	if _, err := s.Update(ctx, webWithMemory("1Gi")); err != nil {
		t.Fatalf("a block with no placements was refused: %v", err)
	}
}

func TestARefusedGrowthNamesTheNodeByHostname(t *testing.T) {
	ctx := context.Background()
	s, _ := newServer(t)
	placeOn(t, ctx, s)
	withNodes(s, "128Mi")
	if _, err := s.St.Put(ctx, join.HostnameKey("n1"), []byte("rack1-a")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(ctx, webWithMemory("1Gi")); err == nil || !strings.Contains(err.Error(), "node rack1-a ") {
		t.Errorf("err %v, want the node named rack1-a", err)
	}
}
