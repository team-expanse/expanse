package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func vaultwardenCfg(kv ...any) map[string]any {
	m := map[string]any{"domain": "https://vault.example.com"}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == nil {
			delete(m, kv[i].(string))
		} else {
			m[kv[i].(string)] = kv[i+1]
		}
	}
	return m
}

func TestVaultwardenSetupAppliesDefaults(t *testing.T) {
	s, err := vaultwardenSetupFrom("/vol", vaultwardenCfg())
	if err != nil {
		t.Fatal(err)
	}
	if s.DataFolder != "/vol/vaultwarden" || s.Port != "18000" || s.SignupsAllowed || s.AdminToken != "" {
		t.Errorf("setup = %+v", s)
	}
}

func TestVaultwardenSetupRejectsBadConfig(t *testing.T) {
	for _, cfg := range []map[string]any{
		vaultwardenCfg("domain", nil),
		vaultwardenCfg("domain", "vault.example.com"),
		vaultwardenCfg("domain", "https://vault.example.com/a b"),
		vaultwardenCfg("adminToken", "a\nb"),
		vaultwardenCfg("settings", map[string]any{"lower_case": "v"}),
		vaultwardenCfg("settings", map[string]any{"SMTP_HOST": "a\nb"}),
		vaultwardenCfg("settings", map[string]any{"SMTP_HOST": map[string]any{}}),
		vaultwardenCfg("settings", "SMTP_HOST=x"),
	} {
		if _, err := vaultwardenSetupFrom("/vol", cfg); err == nil {
			t.Errorf("%v accepted", cfg)
		}
	}
}

func TestVaultwardenEnvKeepsDurableStateOnTheVolume(t *testing.T) {
	s, err := vaultwardenSetupFrom("/vol", vaultwardenCfg("port", 18001.0, "signupsAllowed", true, "adminToken", "$argon2id$x"))
	if err != nil {
		t.Fatal(err)
	}
	got := vaultwardenEnv(s, "/web")
	for _, want := range []string{
		"DATA_FOLDER=/vol/vaultwarden", "ROCKET_ADDRESS=0.0.0.0", "ROCKET_PORT=18001",
		"DOMAIN=https://vault.example.com", "SIGNUPS_ALLOWED=true", "ADMIN_TOKEN=$argon2id$x",
		"WEB_VAULT_FOLDER=/web", "WEB_VAULT_ENABLED=true",
		// A failover is a crash: SQLite must fsync the WAL on every commit.
		"DATABASE_CONN_INIT=PRAGMA busy_timeout = 5000; PRAGMA synchronous = FULL;",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("env lacks %q:\n%v", want, got)
		}
	}
}

func TestVaultwardenEnvWithoutWebVaultOrAdminToken(t *testing.T) {
	s, err := vaultwardenSetupFrom("/vol", vaultwardenCfg())
	if err != nil {
		t.Fatal(err)
	}
	got := vaultwardenEnv(s, "")
	if !slices.Contains(got, "WEB_VAULT_ENABLED=false") || !slices.Contains(got, "SIGNUPS_ALLOWED=false") {
		t.Errorf("env = %v", got)
	}
	for _, kv := range got {
		if strings.HasPrefix(kv, "ADMIN_TOKEN=") {
			t.Errorf("admin token set without one configured: %q", kv)
		}
	}
}

func TestVaultwardenSettingsOverrideTheBlocksOwn(t *testing.T) {
	s, err := vaultwardenSetupFrom("/vol", vaultwardenCfg("settings", map[string]any{
		"SIGNUPS_DOMAINS_WHITELIST": "example.com", "SMTP_PORT": 587.0, "SIGNUPS_ALLOWED": true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	got := vaultwardenEnv(s, "")
	for _, want := range []string{"SIGNUPS_DOMAINS_WHITELIST=example.com", "SMTP_PORT=587", "SIGNUPS_ALLOWED=true"} {
		if !slices.Contains(got, want) {
			t.Errorf("env lacks %q:\n%v", want, got)
		}
	}
	if slices.Contains(got, "SIGNUPS_ALLOWED=false") {
		t.Errorf("the block's SIGNUPS_ALLOWED survived the override: %v", got)
	}
}

func TestFindWebVaultBesideABinDirOnPath(t *testing.T) {
	root := t.TempDir()
	vault := filepath.Join(root, "sw", "share", "vaultwarden", "vault")
	if err := os.MkdirAll(vault, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "none", "bin") + ":" + filepath.Join(root, "sw", "bin")
	if got := findWebVault(path); got != vault {
		t.Errorf("findWebVault = %q, want %q", got, vault)
	}
	if got := findWebVault(filepath.Join(root, "none", "bin")); got != "" {
		t.Errorf("findWebVault without one = %q", got)
	}
}
