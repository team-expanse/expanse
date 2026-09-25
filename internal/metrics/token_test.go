package metrics

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/boltstore"
)

func testStore(t *testing.T) *boltstore.Store {
	t.Helper()
	st, err := boltstore.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("boltstore.New: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestEnsureTokenThenVerify(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	tok, err := EnsureToken(ctx, st)
	if err != nil {
		t.Fatalf("EnsureToken: %v", err)
	}
	if tok == "" {
		t.Fatal("EnsureToken returned empty token on first call")
	}
	if !VerifyToken(ctx, st, tok) {
		t.Error("VerifyToken rejected the token EnsureToken just created")
	}
	if VerifyToken(ctx, st, "wrong-token") {
		t.Error("VerifyToken accepted a wrong token")
	}
}

func TestEnsureTokenIsIdempotent(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	first, err := EnsureToken(ctx, st)
	if err != nil {
		t.Fatalf("EnsureToken (first): %v", err)
	}
	second, err := EnsureToken(ctx, st)
	if err != nil {
		t.Fatalf("EnsureToken (second): %v", err)
	}
	if second != "" {
		t.Error("second EnsureToken call should return empty (record already exists), not a new token")
	}
	if !VerifyToken(ctx, st, first) {
		t.Error("the original token must still verify after a second EnsureToken call")
	}
}

func TestVerifyTokenWithNoRecord(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	if VerifyToken(ctx, st, "anything") {
		t.Error("VerifyToken must treat a missing record as \"wrong token\", not \"no auth required\"")
	}
}

func TestNewTokenRecordRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	rec, err := NewTokenRecord("operator-set-token")
	if err != nil {
		t.Fatalf("NewTokenRecord: %v", err)
	}
	if _, err := st.Put(ctx, store.Key(TokenKey), rec); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !VerifyToken(ctx, st, "operator-set-token") {
		t.Error("a token written via NewTokenRecord (the ctl set-token path) must verify")
	}
}
