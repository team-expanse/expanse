package oidc

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

var testSecret = []byte("01234567890123456789012345678901") // 32+ bytes, HKDF only needs >=32

func TestNewConfigRecordValidation(t *testing.T) {
	cases := []struct {
		name                                        string
		issuer, clientID, clientSecret, redirectURL string
		allow                                       []string
	}{
		{"missing issuer", "", "cid", "secret", "https://ui/callback", []string{"a@example.com"}},
		{"missing client id", "https://idp", "", "secret", "https://ui/callback", []string{"a@example.com"}},
		{"missing client secret", "https://idp", "cid", "", "https://ui/callback", []string{"a@example.com"}},
		{"missing redirect url", "https://idp", "cid", "secret", "", []string{"a@example.com"}},
		{"empty allow-list (fail closed)", "https://idp", "cid", "secret", "https://ui/callback", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewConfigRecord(c.issuer, c.clientID, c.clientSecret, c.redirectURL, c.allow, testSecret)
			if errors.KindOf(err) != errors.KindInvalid {
				t.Fatalf("expected KindInvalid, got %v (err=%v)", errors.KindOf(err), err)
			}
		})
	}
}

func TestConfigRecordRoundTripSealsSecret(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	rec, err := NewConfigRecord("https://idp.example", "my-client", "s3cr3t", "https://ui/login/oidc/callback", []string{"alice@example.com"}, testSecret)
	if err != nil {
		t.Fatalf("NewConfigRecord: %v", err)
	}
	if _, err := st.Put(ctx, store.Key(ConfigKey), rec); err != nil {
		t.Fatalf("Put: %v", err)
	}

	cfg, err := LoadConfig(ctx, st)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Issuer != "https://idp.example" || cfg.ClientID != "my-client" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if len(cfg.AllowedEmails) != 1 || cfg.AllowedEmails[0] != "alice@example.com" {
		t.Fatalf("unexpected allow-list: %v", cfg.AllowedEmails)
	}

	secret, err := cfg.clientSecret(testSecret)
	if err != nil {
		t.Fatalf("clientSecret: %v", err)
	}
	if secret != "s3cr3t" {
		t.Fatalf("unsealed secret = %q, want %q", secret, "s3cr3t")
	}

	// The client secret must never appear in the clear anywhere in the
	// stored record -- only its sealed form.
	if string(cfg.SealedSecret) == "s3cr3t" {
		t.Fatal("client secret stored unsealed")
	}

	// Wrong cluster secret must not unseal it.
	if _, err := cfg.clientSecret([]byte("wrong-cluster-secret-wrong-cluster-secret")); err == nil {
		t.Fatal("clientSecret unsealed with the wrong cluster secret")
	}
}

func TestLoadConfigNotConfigured(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	if _, err := LoadConfig(ctx, st); errors.KindOf(err) != errors.KindNotFound {
		t.Fatalf("expected KindNotFound on an unconfigured store, got %v", err)
	}
	if IsConfigured(ctx, st) {
		t.Fatal("IsConfigured true before any config was written")
	}
}

func TestIsConfiguredAfterWrite(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	rec, err := NewConfigRecord("https://idp.example", "cid", "secret", "https://ui/cb", []string{"a@example.com"}, testSecret)
	if err != nil {
		t.Fatalf("NewConfigRecord: %v", err)
	}
	if _, err := st.Put(ctx, store.Key(ConfigKey), rec); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !IsConfigured(ctx, st) {
		t.Fatal("IsConfigured false after a valid config write")
	}
}
