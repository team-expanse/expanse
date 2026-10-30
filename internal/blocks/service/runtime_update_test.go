package service

import (
	"context"
	"testing"

	"github.com/expanse/expanse/internal/blocks/runtime/systemd"
	"github.com/expanse/expanse/internal/errors"
	pb "github.com/expanse/expanse/proto"
)

func updateRequest(b *pb.Block) string {
	return b.GetMetadata().GetAnnotations()[systemd.RefreshAnnotation]
}

func TestUpdateRuntimeStampsANewRequestEachTime(t *testing.T) {
	ctx := context.Background()
	s, _ := newServer(t)
	if _, err := s.Create(ctx, validBlock("vm")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	a, err := s.UpdateRuntime(ctx, &pb.UpdateRuntimeRequest{Name: "vm"})
	if err != nil || updateRequest(a) == "" {
		t.Fatalf("UpdateRuntime: %v, request %q", err, updateRequest(a))
	}
	b, err := s.UpdateRuntime(ctx, &pb.UpdateRuntimeRequest{Name: "vm"})
	if err != nil || updateRequest(b) == updateRequest(a) {
		t.Errorf("a second update: %v, request %q after %q", err, updateRequest(b), updateRequest(a))
	}
}

func TestUpdateRuntimeRefusesASuspendedOrMissingBlock(t *testing.T) {
	ctx := context.Background()
	s, _ := newServer(t)
	if _, err := s.UpdateRuntime(ctx, &pb.UpdateRuntimeRequest{Name: "nope"}); !errors.Is(err, errors.KindNotFound) {
		t.Errorf("missing block: %v, want not found", err)
	}
	if _, err := s.Create(ctx, validBlock("vm")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.SetPower(ctx, &pb.SetPowerRequest{Name: "vm", Power: pb.Power_POWER_SUSPENDED}); err != nil {
		t.Fatalf("SetPower: %v", err)
	}
	if _, err := s.UpdateRuntime(ctx, &pb.UpdateRuntimeRequest{Name: "vm"}); !errors.Is(err, errors.KindInvalid) {
		t.Errorf("suspended: %v, want invalid", err)
	}
}

func TestAnUpdateKeepsTheRuntimeUpdateRequest(t *testing.T) {
	ctx := context.Background()
	s, _ := newServer(t)
	if _, err := s.Create(ctx, validBlock("vm")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	a, err := s.UpdateRuntime(ctx, &pb.UpdateRuntimeRequest{Name: "vm"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.Update(ctx, validBlock("vm"))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updateRequest(out) != updateRequest(a) {
		t.Errorf("a re-applied manifest changed the request from %q to %q, which would restart the VM", updateRequest(a), updateRequest(out))
	}
}
