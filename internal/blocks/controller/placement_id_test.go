package controller

import (
	"testing"

	"google.golang.org/protobuf/proto"

	pb "github.com/expanse/expanse/proto"
)

func TestEachNewPlacementIsNamed(t *testing.T) {
	c, ctx := runtimeHarness(t)
	runningVM(t, ctx, c)
	mustCreate(t, ctx, c.St, singletonBlock("other"))
	passes(t, ctx, c, 1)
	a := loadStatus(t, ctx, c.St, "default", "vm").GetPlacements()[0].GetPlacementId()
	b := loadStatus(t, ctx, c.St, "default", "other").GetPlacements()[0].GetPlacementId()
	if a == "" || b == "" || a == b {
		t.Fatalf("placement ids %q and %q, want two different ones", a, b)
	}
	if again := passes(t, ctx, c, 2).GetPlacements()[0].GetPlacementId(); again != a {
		t.Errorf("the id changed from %q to %q", a, again)
	}
}

func TestAPlacementFromBeforeIDsGetsOneOnce(t *testing.T) {
	c, ctx := runtimeHarness(t)
	runningVM(t, ctx, c)
	s := loadStatus(t, ctx, c.St, "default", "vm")
	s.Placements[0].PlacementId = ""
	raw, err := proto.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.St.Put(ctx, statusKey(blockKey("default", "vm")), raw); err != nil {
		t.Fatal(err)
	}
	id := passes(t, ctx, c, 1).GetPlacements()[0].GetPlacementId()
	if id == "" {
		t.Fatal("the placement still has no id")
	}
	s = passes(t, ctx, c, 2)
	if p := s.GetPlacements()[0]; p.GetPlacementId() != id || p.GetPhase() != pb.Phase_RUNNING {
		t.Errorf("later: id %q phase %v, want %q RUNNING", p.GetPlacementId(), p.GetPhase(), id)
	}
}
