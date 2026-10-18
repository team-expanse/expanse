package secrets

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/boltstore"
)

func newStore(t *testing.T) (*Store, store.Store) {
	t.Helper()
	st, err := boltstore.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Store{St: st, ClusterSecret: bytes.Repeat([]byte{7}, 32)}, st
}

func TestPutSealsAndGetUnseals(t *testing.T) {
	ctx := context.Background()
	s, st := newStore(t)
	op, err := s.PutOp("default", "db.superuserPassword.ab12", []byte("hunter2-plain"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Txn(ctx, []store.Op{op}); err != nil {
		t.Fatal(err)
	}
	e, err := st.Get(ctx, Key("default", "db.superuserPassword.ab12"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(e.Value, []byte("hunter2-plain")) {
		t.Fatal("the store holds the value in plain text")
	}
	got, err := s.Get(ctx, "default", "db.superuserPassword.ab12")
	if err != nil || string(got) != "hunter2-plain" {
		t.Fatalf("Get = %q, %v", got, err)
	}

	other := &Store{St: st, ClusterSecret: bytes.Repeat([]byte{8}, 32)}
	if _, err := other.Get(ctx, "default", "db.superuserPassword.ab12"); err == nil {
		t.Error("another cluster secret unsealed the value")
	}
}

func TestGetMissingIsNotFound(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.Get(context.Background(), "default", "nope"); !errors.Is(err, errors.KindNotFound) {
		t.Fatalf("err = %v, want not found", err)
	}
}

func TestDeleteOpRemovesTheSecret(t *testing.T) {
	ctx := context.Background()
	s, st := newStore(t)
	op, _ := s.PutOp("ns", "a", []byte("v"))
	if _, err := st.Txn(ctx, []store.Op{op}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Txn(ctx, []store.Op{DeleteOp("ns", "a")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "ns", "a"); !errors.Is(err, errors.KindNotFound) {
		t.Fatalf("err = %v, want not found", err)
	}
}

func TestNewNameIsValidAndUnique(t *testing.T) {
	a, b := NewName("db", "superuserPassword"), NewName("db", "superuserPassword")
	if a == b {
		t.Fatalf("two names alike: %s", a)
	}
	for _, n := range []string{a, b} {
		if !ValidName(n) {
			t.Errorf("%q is not a valid name", n)
		}
	}
	for _, bad := range []string{"", "a/b", "../x", "a b", string(make([]byte, 254))} {
		if ValidName(bad) {
			t.Errorf("%q passed ValidName", bad)
		}
	}
}

func TestStoreWithoutAClusterSecretRefusesToSeal(t *testing.T) {
	s, _ := newStore(t)
	s.ClusterSecret = nil
	if _, err := s.PutOp("ns", "a", []byte("v")); err == nil {
		t.Fatal("sealed without a cluster secret")
	}
}
