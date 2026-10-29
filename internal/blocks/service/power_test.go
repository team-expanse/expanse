package service

import (
	"context"
	"testing"

	"github.com/expanse/expanse/internal/errors"
	pb "github.com/expanse/expanse/proto"
)

func TestSetPowerSuspendsAndResumes(t *testing.T) {
	ctx := context.Background()
	s, _ := newServer(t)
	if _, err := s.Create(ctx, validBlock("vm")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	out, err := s.SetPower(ctx, &pb.SetPowerRequest{Name: "vm", Power: pb.Power_POWER_SUSPENDED})
	if err != nil {
		t.Fatalf("SetPower: %v", err)
	}
	if out.GetSpec().GetPower() != pb.Power_POWER_SUSPENDED || out.GetSpec().GetType() != "util/echo" {
		t.Fatalf("spec after suspend: %+v", out.GetSpec())
	}
	got, _ := s.Get(ctx, &pb.GetBlockRequest{Namespace: "default", Name: "vm"})
	if got.GetSpec().GetPower() != pb.Power_POWER_SUSPENDED {
		t.Errorf("stored power = %v", got.GetSpec().GetPower())
	}
	if out, err = s.SetPower(ctx, &pb.SetPowerRequest{Name: "vm", Power: pb.Power_POWER_ON}); err != nil || out.GetSpec().GetPower() != pb.Power_POWER_ON {
		t.Errorf("resume: %v, power %v", err, out.GetSpec().GetPower())
	}
}

func TestSetPowerRefusesUnsuspendableTypesAndMissingBlocks(t *testing.T) {
	ctx := context.Background()
	s, _ := newServer(t)
	if _, err := s.SetPower(ctx, &pb.SetPowerRequest{Name: "nope", Power: pb.Power_POWER_SUSPENDED}); !errors.Is(err, errors.KindNotFound) {
		t.Errorf("missing block: %v, want not found", err)
	}
	if _, err := s.SetPower(ctx, &pb.SetPowerRequest{Name: "", Power: pb.Power_POWER_ON}); !errors.Is(err, errors.KindInvalid) {
		t.Errorf("no name: %v, want invalid", err)
	}
	if _, err := s.SetPower(ctx, &pb.SetPowerRequest{Name: "x", Power: pb.Power_POWER_UNSPECIFIED}); !errors.Is(err, errors.KindInvalid) {
		t.Errorf("no power: %v, want invalid", err)
	}
}

func TestUpdateWithoutPowerKeepsTheBlockSuspended(t *testing.T) {
	ctx := context.Background()
	s, _ := newServer(t)
	if _, err := s.Create(ctx, validBlock("vm")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.SetPower(ctx, &pb.SetPowerRequest{Name: "vm", Power: pb.Power_POWER_SUSPENDED}); err != nil {
		t.Fatalf("SetPower: %v", err)
	}
	out, err := s.Update(ctx, validBlock("vm"))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if out.GetSpec().GetPower() != pb.Power_POWER_SUSPENDED {
		t.Errorf("an update without power resumed the block: %v", out.GetSpec().GetPower())
	}
}
