package auth

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/expanse/expanse/internal/errors"
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

func TestIssueAndGetSession(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	sess, err := IssueSession(ctx, st, "admin")
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}
	if sess.ID == "" || sess.CSRFToken == "" {
		t.Fatal("IssueSession returned an empty ID or CSRF token")
	}

	got, err := GetSession(ctx, st, sess.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.UserID != "admin" || got.CSRFToken != sess.CSRFToken {
		t.Fatalf("GetSession returned %+v, want a match for %+v", got, sess)
	}
}

func TestGetSessionUnknownID(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	if _, err := GetSession(ctx, st, "does-not-exist"); !errors.Is(err, errors.KindNotFound) {
		t.Fatalf("GetSession on unknown ID: got %v, want KindNotFound", err)
	}
}

func TestSessionExpiry(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	sess, err := IssueSession(ctx, st, "admin")
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}
	// Backdate the record directly in the store to simulate expiry
	// without sleeping DefaultSessionTTL in a test.
	rec := `{"user":"admin","csrf":"x","exp":1,"created":1}`
	if _, err := st.Put(ctx, store.Key(SessionKeyPrefix+sess.ID), []byte(rec)); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	if _, err := GetSession(ctx, st, sess.ID); !errors.Is(err, errors.KindNotFound) {
		t.Fatalf("GetSession on expired session: got %v, want KindNotFound", err)
	}
	// Expiry deletes as a side effect: a second lookup must also miss.
	if _, err := GetSession(ctx, st, sess.ID); !errors.Is(err, errors.KindNotFound) {
		t.Fatal("expired session record was not cleaned up")
	}
}

func TestInvalidateSession(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	sess, err := IssueSession(ctx, st, "admin")
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}
	if err := InvalidateSession(ctx, st, sess.ID); err != nil {
		t.Fatalf("InvalidateSession: %v", err)
	}
	if _, err := GetSession(ctx, st, sess.ID); !errors.Is(err, errors.KindNotFound) {
		t.Fatalf("GetSession after logout: got %v, want KindNotFound", err)
	}
	// Invalidating twice (double logout, or a stale tab) is not an error.
	if err := InvalidateSession(ctx, st, sess.ID); err != nil {
		t.Fatalf("second InvalidateSession: %v", err)
	}
}

func TestSessionsAreIndependentlyUnguessable(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	a, err := IssueSession(ctx, st, "admin")
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}
	b, err := IssueSession(ctx, st, "admin")
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}
	if a.ID == b.ID || a.CSRFToken == b.CSRFToken {
		t.Fatal("two sessions issued the same ID or CSRF token")
	}
}
