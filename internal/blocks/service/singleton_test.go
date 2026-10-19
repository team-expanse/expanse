package service

import (
	"context"
	"testing"

	pb "github.com/expanse/expanse/proto"
)

// A singleton written without replicas is stored with replicas: 1, which the scheduler places.
func TestSingletonWithoutReplicasIsStoredWithOne(t *testing.T) {
	ctx := context.Background()
	s, _ := newSecretServer(t)
	b := pgBlock(t, map[string]any{})
	b.Spec.Replicas = nil
	if _, err := s.Create(ctx, b); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx, &pb.GetBlockRequest{Namespace: "default", Name: "db"})
	if got.GetSpec().Replicas == nil || got.GetSpec().GetReplicas() != 1 {
		t.Fatalf("replicas = %v", got.GetSpec().Replicas)
	}
	got.Spec.Replicas = nil
	if _, err := s.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.Get(ctx, &pb.GetBlockRequest{Namespace: "default", Name: "db"}); got.GetSpec().GetReplicas() != 1 {
		t.Fatalf("after update, replicas = %v", got.GetSpec().Replicas)
	}
}
