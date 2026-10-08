package main

import (
	"encoding/base64"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func forgejoCfg(kv ...any) map[string]any {
	m := map[string]any{
		"rootURL": "http://git.example.com/", "adminUser": "gitadmin",
		"adminPassword": "s3cret-pass", "adminEmail": "ops@example.com",
	}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == nil {
			delete(m, kv[i].(string))
		} else {
			m[kv[i].(string)] = kv[i+1]
		}
	}
	return m
}

func TestForgejoSetupAppliesDefaults(t *testing.T) {
	s, err := forgejoSetupFrom("/vol", forgejoCfg("rootURL", "https://git.example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Port != "13000" || s.SSHPort != "12222" || s.PublicSSHPort != "2222" {
		t.Errorf("ports = %s/%s/%s, want 13000/12222/2222", s.Port, s.SSHPort, s.PublicSSHPort)
	}
	if s.RootURL != "https://git.example.com/" || s.Domain != "git.example.com" {
		t.Errorf("rootURL/domain = %q/%q", s.RootURL, s.Domain)
	}
	if s.WorkPath != "/vol/forgejo" || s.AllowRegistration {
		t.Errorf("workPath = %q, allowRegistration = %v", s.WorkPath, s.AllowRegistration)
	}
}

func TestForgejoSetupRejectsBadConfig(t *testing.T) {
	for _, cfg := range []map[string]any{
		forgejoCfg("rootURL", nil),
		forgejoCfg("rootURL", "git.example.com"),
		forgejoCfg("rootURL", "http://git.example.com/x y"),
		forgejoCfg("adminUser", nil),
		forgejoCfg("adminUser", "a b"),
		forgejoCfg("adminPassword", "short"),
		forgejoCfg("adminEmail", "nope"),
		forgejoCfg("settings", map[string]any{"bad section": map[string]any{"K": "v"}}),
		forgejoCfg("settings", map[string]any{"server": map[string]any{"K": "a\nb"}}),
		forgejoCfg("settings", map[string]any{"server": "v"}),
	} {
		if _, err := forgejoSetupFrom("/vol", cfg); err == nil {
			t.Errorf("%v accepted", cfg)
		}
	}
}

func TestForgejoINIKeepsStateOnTheVolume(t *testing.T) {
	s, err := forgejoSetupFrom("/vol", forgejoCfg("port", 13001.0, "publicSSHPort", 22.0))
	if err != nil {
		t.Fatal(err)
	}
	got := forgejoINI(s, "blk", forgejoSecrets{SecretKey: "sk", InternalToken: "it", JWT: "jw", LFSJWT: "lj"})
	for _, want := range []string{
		"RUN_USER = blk\nWORK_PATH = /vol/forgejo\n",
		"HTTP_PORT = 13001\n", "ROOT_URL = http://git.example.com/\n", "DOMAIN = git.example.com\n",
		"SSH_LISTEN_PORT = 12222\n", "SSH_PORT = 22\n", "BUILTIN_SSH_SERVER_USER = git\n",
		"APP_DATA_PATH = /vol/forgejo/data\n", "LFS_JWT_SECRET = lj\n",
		"[database]\nDB_TYPE = sqlite3\nPATH = /vol/forgejo/data/forgejo.db\n",
		"[repository]\nROOT = /vol/forgejo/repositories\n",
		"INSTALL_LOCK = true\nSECRET_KEY = sk\nINTERNAL_TOKEN = it\n", "JWT_SECRET = jw\n",
		"DISABLE_REGISTRATION = true\n", "[session]\nPROVIDER = file\n",
		// A failover is a crash: git must fsync what it acknowledges.
		"[git.config]\ncore.fsync = all\ncore.fsyncMethod = fsync\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("app.ini lacks %q:\n%s", want, got)
		}
	}
}

func TestForgejoINIAppliesSettingsLast(t *testing.T) {
	s, err := forgejoSetupFrom("/vol", forgejoCfg("settings", map[string]any{
		"database": map[string]any{"DB_TYPE": "mysql", "HOST": "10.0.0.9:3306"},
		"mailer":   map[string]any{"ENABLED": "false"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	got := forgejoINI(s, "blk", forgejoSecrets{})
	if strings.Count(got, "DB_TYPE") != 1 || !strings.Contains(got, "DB_TYPE = mysql\nPATH = ") {
		t.Errorf("DB_TYPE not replaced in place:\n%s", got)
	}
	if !strings.Contains(got, "HOST = 10.0.0.9:3306\n") || !strings.Contains(got, "\n[mailer]\nENABLED = false\n") {
		t.Errorf("settings not applied:\n%s", got)
	}
}

func TestForgejoSecretsPersistAcrossStarts(t *testing.T) {
	dir := t.TempDir()
	first, err := loadOrCreateForgejoSecrets(dir)
	if err != nil {
		t.Fatal(err)
	}
	again, err := loadOrCreateForgejoSecrets(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first != again {
		t.Errorf("secrets changed across starts: %+v vs %+v", first, again)
	}
	for _, jwt := range []string{first.JWT, first.LFSJWT} {
		if raw, err := base64.RawURLEncoding.DecodeString(jwt); err != nil || len(raw) != 32 {
			t.Errorf("JWT secret %q is not 32 base64url bytes (%v)", jwt, err)
		}
	}
	if first.SecretKey == "" || first.InternalToken == "" || first.SecretKey == first.InternalToken {
		t.Errorf("secrets = %+v", first)
	}
	if _, err := loadOrCreateSecret(filepath.Join(dir, "secret_key")); err != nil {
		t.Errorf("secret_key not stored in %s: %v", dir, err)
	}
}

func TestEnsureForgejoAdmin(t *testing.T) {
	s, _ := forgejoSetupFrom("/vol", forgejoCfg())
	for _, tc := range []struct {
		name      string
		createOut string
		createErr error
		wantCalls []string
		wantErr   bool
	}{
		{"new", "created", nil, []string{"create"}, false},
		{
			"exists", "Command error: CreateUser: user already exists [name: gitadmin]", errors.New("exit 1"),
			[]string{"create", "change-password"},
			false,
		},
		{"broken", "database is locked", errors.New("exit 1"), []string{"create"}, true},
	} {
		var calls []string
		run := func(args ...string) (string, error) {
			calls = append(calls, args[2])
			if args[2] == "create" {
				return tc.createOut, tc.createErr
			}
			return "", nil
		}
		err := ensureForgejoAdmin(s, run)
		if (err != nil) != tc.wantErr || strings.Join(calls, ",") != strings.Join(tc.wantCalls, ",") {
			t.Errorf("%s: calls %v, err %v", tc.name, calls, err)
		}
	}
}

// The unit's DynamicUser has no home, and Forgejo exits without one.
func TestForgejoEnvSetsHome(t *testing.T) {
	env := forgejoEnv("/tmp/expblk-x")
	if !slices.Contains(env, "HOME=/tmp/expblk-x") {
		t.Errorf("env = %v, want HOME=/tmp/expblk-x", env)
	}
}
