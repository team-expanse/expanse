package service

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/expanse/expanse/internal/blocks/validate"
	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/secrets"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/boltstore"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// secretCatalog has one type, db/pg, whose password field is writeOnly.
type secretCatalog struct{ fakeCatalog }

func (secretCatalog) HasType(t string) bool   { return t == "db/pg" }
func (secretCatalog) Types() []string         { return []string{"db/pg"} }
func (secretCatalog) Suspendable(string) bool { return false }
func (secretCatalog) WriteOnlyFields(t string) []string {
	if t == "db/pg" {
		return []string{"password"}
	}
	return nil
}

func newSecretServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	st, err := boltstore.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := New(st, func() validate.Context { return validate.Context{Catalog: secretCatalog{}} })
	s.Secrets = &secrets.Store{St: st, ClusterSecret: bytes.Repeat([]byte{1}, 32)}
	return s, st
}

func pgBlock(t *testing.T, cfg map[string]any) *pb.Block {
	t.Helper()
	c, err := structpb.NewStruct(cfg)
	if err != nil {
		t.Fatal(err)
	}
	one := int32(1)
	return &pb.Block{
		Metadata: &pb.Metadata{Name: "db", Namespace: "default"},
		Spec: &pb.BlockSpec{
			Type: "db/pg", Replicas: &one, Config: c,
			Strategy: &pb.Strategy{Kind: pb.StrategyKind_SINGLETON},
		},
	}
}

// storeHolds reports whether any stored value contains v.
func storeHolds(t *testing.T, st store.Store, v string) bool {
	t.Helper()
	all, err := st.List(context.Background(), "/")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range all {
		if bytes.Contains(e.Value, []byte(v)) {
			return true
		}
	}
	return false
}

func secretRef(t *testing.T, s *Server) *pb.SecretRef {
	t.Helper()
	b, err := s.Get(context.Background(), &pb.GetBlockRequest{Namespace: "default", Name: "db"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := b.GetSpec().GetConfig().GetFields()["password"]; ok {
		t.Fatal("the stored config holds the password")
	}
	refs := b.GetSpec().GetSecrets()
	if len(refs) != 1 || refs[0].GetPath() != "password" {
		t.Fatalf("secret refs = %v", refs)
	}
	return refs[0]
}

func TestCreateSealsWriteOnlyFields(t *testing.T) {
	ctx := context.Background()
	s, st := newSecretServer(t)
	if _, err := s.Create(ctx, pgBlock(t, map[string]any{"password": "pg-plain-pass", "port": 5432})); err != nil {
		t.Fatal(err)
	}
	if storeHolds(t, st, "pg-plain-pass") {
		t.Fatal("the password is in the store in plain text")
	}
	ref := secretRef(t, s)
	if v, err := s.Secrets.Get(ctx, "default", ref.GetName()); err != nil || string(v) != "pg-plain-pass" {
		t.Fatalf("secret = %q, %v", v, err)
	}
}

func TestUpdateKeepsReplacesAndDropsSecrets(t *testing.T) {
	ctx := context.Background()
	s, _ := newSecretServer(t)
	if _, err := s.Create(ctx, pgBlock(t, map[string]any{"password": "first-pass"})); err != nil {
		t.Fatal(err)
	}
	first := secretRef(t, s)

	// Left out: kept, with the same name, so the replicas do not restart.
	if _, err := s.Update(ctx, pgBlock(t, map[string]any{"port": 1})); err != nil {
		t.Fatal(err)
	}
	if got := secretRef(t, s); got.GetName() != first.GetName() {
		t.Fatalf("an omitted password changed the ref: %s -> %s", first.GetName(), got.GetName())
	}

	// Given again unchanged (a re-applied YAML): kept as well.
	if _, err := s.Update(ctx, pgBlock(t, map[string]any{"password": "first-pass"})); err != nil {
		t.Fatal(err)
	}
	if got := secretRef(t, s); got.GetName() != first.GetName() {
		t.Fatalf("an unchanged password changed the ref: %s -> %s", first.GetName(), got.GetName())
	}

	// Given a new value: a new secret under a new name. The old one stays until the
	// block is deleted, for a generation rollback to the spec that names it.
	if _, err := s.Update(ctx, pgBlock(t, map[string]any{"password": "second-pass"})); err != nil {
		t.Fatal(err)
	}
	second := secretRef(t, s)
	if second.GetName() == first.GetName() {
		t.Fatal("a new password kept the old name")
	}
	if v, _ := s.Secrets.Get(ctx, "default", second.GetName()); string(v) != "second-pass" {
		t.Errorf("secret = %q", v)
	}
	if ok, _ := s.Secrets.Exists(ctx, "default", first.GetName()); !ok {
		t.Error("the replaced secret is gone; a rollback to the old spec could not start")
	}

	// Set to "": removed.
	if _, err := s.Update(ctx, pgBlock(t, map[string]any{"password": ""})); err != nil {
		t.Fatal(err)
	}
	b, _ := s.Get(ctx, &pb.GetBlockRequest{Namespace: "default", Name: "db"})
	if len(b.GetSpec().GetSecrets()) != 0 {
		t.Errorf("refs after clearing: %v", b.GetSpec().GetSecrets())
	}
	if ok, _ := s.Secrets.Exists(ctx, "default", second.GetName()); !ok {
		t.Error("the cleared secret is gone; a rollback to the old spec could not start")
	}

	// Delete removes every secret the block ever had.
	if _, err := s.Delete(ctx, &pb.DeleteBlockRequest{Namespace: "default", Name: "db"}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{first.GetName(), second.GetName()} {
		if ok, _ := s.Secrets.Exists(ctx, "default", n); ok {
			t.Errorf("%s outlived its block", n)
		}
	}
}

func TestDeleteRemovesTheBlocksSecrets(t *testing.T) {
	ctx := context.Background()
	s, _ := newSecretServer(t)
	if _, err := s.Create(ctx, pgBlock(t, map[string]any{"password": "pass"})); err != nil {
		t.Fatal(err)
	}
	ref := secretRef(t, s)
	if _, err := s.Delete(ctx, &pb.DeleteBlockRequest{Namespace: "default", Name: "db"}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Secrets.Exists(ctx, "default", ref.GetName()); ok {
		t.Error("the deleted block's secret is still stored")
	}
}

func TestARefToAMissingSecretIsRefused(t *testing.T) {
	s, _ := newSecretServer(t)
	b := pgBlock(t, map[string]any{})
	b.Spec.Secrets = []*pb.SecretRef{{Name: "nope", Path: "password"}}
	if _, err := s.Create(context.Background(), b); !errors.Is(err, errors.KindInvalid) {
		t.Fatalf("err = %v, want invalid", err)
	}
}

func TestWithoutASecretStoreSecretsStayInTheConfig(t *testing.T) {
	ctx := context.Background()
	s, _ := newSecretServer(t)
	s.Secrets = nil
	if _, err := s.Create(ctx, pgBlock(t, map[string]any{"password": "p"})); err != nil {
		t.Fatal(err)
	}
	b, _ := s.Get(ctx, &pb.GetBlockRequest{Namespace: "default", Name: "db"})
	if b.GetSpec().GetConfig().GetFields()["password"].GetStringValue() != "p" || len(b.GetSpec().GetSecrets()) != 0 {
		t.Fatalf("spec = %v", b.GetSpec())
	}
}
