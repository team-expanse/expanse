package auth

import (
	"context"
	"strings"
	"testing"
)

func TestEnsureAdminCreatesOnce(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	pw, err := EnsureAdmin(ctx, st)
	if err != nil {
		t.Fatalf("EnsureAdmin: %v", err)
	}
	if pw == "" {
		t.Fatal("first EnsureAdmin call returned no password")
	}
	if !VerifyAdminPassword(ctx, st, pw) {
		t.Fatal("generated password does not verify against the record it created")
	}

	// A second call (e.g. a different node racing at startup) must not
	// overwrite the account or hand back another password.
	pw2, err := EnsureAdmin(ctx, st)
	if err != nil {
		t.Fatalf("second EnsureAdmin: %v", err)
	}
	if pw2 != "" {
		t.Fatal("second EnsureAdmin call returned a non-empty password; only the creator should")
	}
	if !VerifyAdminPassword(ctx, st, pw) {
		t.Fatal("original password stopped working after a second EnsureAdmin call")
	}
}

func TestVerifyAdminPasswordWrongPassword(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	if _, err := EnsureAdmin(ctx, st); err != nil {
		t.Fatalf("EnsureAdmin: %v", err)
	}
	if VerifyAdminPassword(ctx, st, "definitely-wrong") {
		t.Fatal("VerifyAdminPassword accepted a wrong password")
	}
}

func TestVerifyAdminPasswordNoAccount(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	if VerifyAdminPassword(ctx, st, "anything") {
		t.Fatal("VerifyAdminPassword must not authorize when no admin account exists yet")
	}
}

func TestSetAdminPasswordReset(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	if _, err := EnsureAdmin(ctx, st); err != nil {
		t.Fatalf("EnsureAdmin: %v", err)
	}
	if err := SetAdminPassword(ctx, st, "new-password"); err != nil {
		t.Fatalf("SetAdminPassword: %v", err)
	}
	if !VerifyAdminPassword(ctx, st, "new-password") {
		t.Fatal("reset password does not verify")
	}
}

func TestAdminRecordStoresNoPlaintext(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	pw, err := EnsureAdmin(ctx, st)
	if err != nil {
		t.Fatalf("EnsureAdmin: %v", err)
	}
	e, err := st.Get(ctx, AdminKey)
	if err != nil {
		t.Fatalf("Get(AdminKey): %v", err)
	}
	if strings.Contains(string(e.Value), pw) {
		t.Fatalf("admin store record contains the plaintext password: %s", e.Value)
	}
}
