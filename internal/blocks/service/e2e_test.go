package service

import (
	"bytes"
	"context"
	"testing"

	"github.com/expanse/expanse/internal/blocks/apply"
	"github.com/expanse/expanse/internal/blocks/catalog"
	"github.com/expanse/expanse/internal/blocks/validate"
	pb "github.com/expanse/expanse/proto"
)

const blockYAML = `apiVersion: expanse.io/v1
kind: Block
metadata:
  name: web
  namespace: default
spec:
  type: util/echo
  replicas: 1
  resources:
    requests:
      cpu: 100m
      memory: 64Mi
`

// TestEndToEndApplyListGetScaleDelete drives the §7 CLI surface in-proc:
// apply → list → get → scale --replicas N → delete, asserting store state
// after each step.
func TestEndToEndApplyListGetScaleDelete(t *testing.T) {
	ctx := context.Background()
	s, rev := newServer(t)
	rev0, err := rev(ctx)
	if err != nil {
		t.Fatalf("revision: %v", err)
	}

	// --- apply (parse YAML → Create) ---
	b, err := apply.Parse(bytes.NewReader([]byte(blockYAML)))
	if err != nil {
		t.Fatalf("apply.Parse: %v", err)
	}
	if b.GetSpec().GetType() != "util/echo" || b.GetMetadata().GetName() != "web" {
		t.Fatalf("parsed block = %+v", b)
	}
	created, err := s.Create(ctx, b)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rev1, _ := rev(ctx)
	if rev1 != rev0+1 {
		t.Errorf("revision %d -> %d after apply, want +1 (one txn: block+generation)", rev0, rev1)
	}
	if created.GetStatus().GetPhase() != pb.Phase_PENDING {
		t.Errorf("phase after apply = %v, want PENDING", created.GetStatus().GetPhase())
	}

	// --- list ---
	lst, err := s.List(ctx, &pb.ListBlocksRequest{Namespace: "default"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(lst.GetBlocks()) != 1 || lst.GetBlocks()[0].GetMetadata().GetName() != "web" {
		t.Errorf("list after apply = %+v", lst.GetBlocks())
	}

	// --- get ---
	got, err := s.Get(ctx, &pb.GetBlockRequest{Namespace: "default", Name: "web"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.GetSpec().GetResources().GetRequests().GetCpu() != "100m" {
		t.Errorf("get spec.cpu = %q, want 100m", got.GetSpec().GetResources().GetRequests().GetCpu())
	}

	// --- scale --replicas 2 ---
	scaled, err := s.Scale(ctx, &pb.ScaleRequest{Namespace: "default", Name: "web", Replicas: 2})
	if err != nil {
		t.Fatalf("Scale: %v", err)
	}
	if scaled.GetSpec().GetReplicas() != 2 {
		t.Errorf("scaled replicas = %d, want 2", scaled.GetSpec().GetReplicas())
	}
	stored, _ := s.Get(ctx, &pb.GetBlockRequest{Namespace: "default", Name: "web"})
	if stored.GetSpec().GetReplicas() != 2 {
		t.Errorf("stored replicas after scale = %d, want 2", stored.GetSpec().GetReplicas())
	}
	if stored.GetStatus().GetPhase() != pb.Phase_PENDING {
		t.Errorf("phase preserved after scale = %v, want PENDING", stored.GetStatus().GetPhase())
	}

	// Scale to an invalid count is rejected with zero store writes:
	// scale back to 1, make the block a singleton, then scaling to 2
	// violates V5.
	if _, err := s.Scale(ctx, &pb.ScaleRequest{Namespace: "default", Name: "web", Replicas: 1}); err != nil {
		t.Fatalf("Scale to 1: %v", err)
	}
	cur, _ := s.Get(ctx, &pb.GetBlockRequest{Namespace: "default", Name: "web"})
	cur.Spec.Strategy = &pb.Strategy{Kind: pb.StrategyKind_SINGLETON}
	if _, err := s.Update(ctx, cur); err != nil {
		t.Fatalf("Update to singleton: %v", err)
	}
	revS, _ := rev(ctx)
	if _, err := s.Scale(ctx, &pb.ScaleRequest{Namespace: "default", Name: "web", Replicas: 2}); err == nil {
		t.Error("scale of singleton to 2 accepted (V5)")
	}
	revS2, _ := rev(ctx)
	if revS2 != revS {
		t.Errorf("revision %d -> %d on rejected scale, want unchanged", revS, revS2)
	}

	// --- delete ---
	if _, err := s.Delete(ctx, &pb.DeleteBlockRequest{Namespace: "default", Name: "web"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	lst2, err := s.List(ctx, &pb.ListBlocksRequest{Namespace: "default"})
	if err != nil {
		t.Fatalf("List after delete: %v", err)
	}
	if len(lst2.GetBlocks()) != 0 {
		t.Errorf("blocks after delete = %+v, want none", lst2.GetBlocks())
	}
}

// TestDryRunMakesZeroWrites: --dry-run is Parse + admission Validate —
// no store access at all, so the store revision cannot move and the key
// is never created.
func TestDryRunMakesZeroWrites(t *testing.T) {
	ctx := context.Background()
	s, rev := newServer(t)
	rev0, _ := rev(ctx)

	b, err := apply.Parse(bytes.NewReader([]byte(blockYAML)))
	if err != nil {
		t.Fatalf("apply.Parse: %v", err)
	}
	// Dry-run = admission pass only (this is exactly what the CLI runs).
	ad := s.adCtx()
	ad.Existing = s.namesInNamespace(ctx, "default", "")
	if ves := validateDryRun(b, ad); len(ves) > 0 {
		t.Fatalf("dry-run rejected valid block: %v", ves)
	}

	rev1, _ := rev(ctx)
	if rev1 != rev0 {
		t.Errorf("revision %d -> %d after dry-run, want unchanged", rev0, rev1)
	}
	if _, err := s.Get(ctx, &pb.GetBlockRequest{Namespace: "default", Name: "web"}); err == nil {
		t.Error("dry-run wrote the block to the store")
	}

	// Dry-run on an invalid block also fails without touching anything.
	bad, err := apply.Parse(bytes.NewReader([]byte(`metadata:
  name: bad
spec:
  type: util/echo
  replicas: 3
`)))
	if err != nil {
		t.Fatalf("parse bad: %v", err)
	}
	if ves := validateDryRun(bad, ad); len(ves) == 0 {
		t.Error("dry-run accepted primary-replica with 3 replicas (V7)")
	}
	rev2, _ := rev(ctx)
	if rev2 != rev0 {
		t.Errorf("revision moved after invalid dry-run")
	}
}

// TestRestartStampsAnnotationAndPreservesSpec.
func TestRestartStampsAnnotationAndPreservesSpec(t *testing.T) {
	ctx := context.Background()
	s, _ := newServer(t)
	if _, err := s.Create(ctx, validBlock("web")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	out, err := s.Restart(ctx, &pb.RestartRequest{Namespace: "default", Name: "web"})
	if err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if out.GetMetadata().GetAnnotations()["expanse.io/restartedAt"] == "" {
		t.Errorf("restartedAt annotation missing: %v", out.GetMetadata().GetAnnotations())
	}
	if out.GetSpec().GetType() != "util/echo" {
		t.Errorf("spec clobbered by restart: %+v", out.GetSpec())
	}
	if _, err := s.Restart(ctx, &pb.RestartRequest{Namespace: "default", Name: "nope"}); err == nil {
		t.Error("restart of missing block succeeded")
	}
}

// validateDryRun is the CLI --dry-run entry: admission Validate only.
func validateDryRun(b *pb.Block, ad validate.Context) []validate.ValidationError {
	return validate.Validate(b, ad)
}

// TestCatalogServerAgainstRealEcho lists and shows T05's real util/echo
// entry from nix/blocks.
func TestCatalogServerAgainstRealEcho(t *testing.T) {
	cat, err := catalog.Load("../../../nix/blocks")
	if err != nil {
		t.Fatalf("catalog.Load: %v", err)
	}
	cs := NewCatalogServer(cat)
	lst, err := cs.ListTypes(context.Background(), &pb.ListTypesRequest{})
	if err != nil {
		t.Fatalf("ListTypes: %v", err)
	}
	found := false
	for _, ty := range lst.GetTypes() {
		if ty.GetName() == "util/echo" {
			found = true
			if ty.GetVersion() == "" || len(ty.GetSchemaJson()) == 0 {
				t.Errorf("util/echo type incomplete: version=%q schema=%d bytes",
					ty.GetVersion(), len(ty.GetSchemaJson()))
			}
		}
	}
	if !found {
		t.Error("util/echo missing from ListTypes")
	}
	got, err := cs.GetType(context.Background(), &pb.GetTypeRequest{Name: "util/echo"})
	if err != nil {
		t.Fatalf("GetType: %v", err)
	}
	if got.GetSchemaJson()[0] != '{' {
		t.Errorf("schema_json is not JSON: %q", got.GetSchemaJson()[:20])
	}
	if _, err := cs.GetType(context.Background(), &pb.GetTypeRequest{Name: "web/nope"}); err == nil {
		t.Error("unknown type returned without error")
	}
}
