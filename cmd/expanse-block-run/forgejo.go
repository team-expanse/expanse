package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Mirrors nix/blocks/dev/forgejo/schema.json, so app.ini is never built from unchecked text.
var (
	forgejoRootURL = regexp.MustCompile(`^https?://([A-Za-z0-9.-]+|\[[0-9A-Fa-f:]+\])(:[0-9]{1,5})?(/[A-Za-z0-9._~/-]*)?$`)
	forgejoUser    = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,40}$`)
	forgejoEmail   = regexp.MustCompile(`^[^@\s]+@[^@\s]+$`)
	forgejoININame = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	forgejoINIVal  = regexp.MustCompile(`^[^\n\r]*$`)
)

// forgejoSetup is everything one dev/forgejo instance needs.
type forgejoSetup struct {
	WorkPath, Port, SSHPort, PublicSSHPort string
	RootURL, Domain                        string
	AdminUser, AdminPassword, AdminEmail   string
	AllowRegistration                      bool
	Settings                               map[string]map[string]string
}

// forgejoSecrets are generated once and kept on the volume, so sessions and tokens survive failover.
type forgejoSecrets struct {
	SecretKey, InternalToken, JWT, LFSJWT string
}

// forgejoSetupFrom applies the block's defaults to spec.config and validates it.
func forgejoSetupFrom(mountPath string, cfg map[string]any) (forgejoSetup, error) {
	s := forgejoSetup{
		WorkPath:          filepath.Join(mountPath, "forgejo"),
		Port:              cfgPortOr(cfg, "13000"),
		SSHPort:           fmt.Sprint(cfgInt(cfg, "sshPort", 12222)),
		PublicSSHPort:     fmt.Sprint(cfgInt(cfg, "publicSSHPort", 2222)),
		RootURL:           cfgStr(cfg, "rootURL"),
		AdminUser:         cfgStr(cfg, "adminUser"),
		AdminPassword:     cfgStr(cfg, "adminPassword"),
		AdminEmail:        cfgStr(cfg, "adminEmail"),
		AllowRegistration: cfgBool(cfg, "allowRegistration", false),
	}
	switch {
	case !forgejoRootURL.MatchString(s.RootURL):
		return forgejoSetup{}, fmt.Errorf("dev/forgejo: rootURL %q must be an http(s) URL", s.RootURL)
	case !forgejoUser.MatchString(s.AdminUser):
		return forgejoSetup{}, fmt.Errorf("dev/forgejo: adminUser %q must match %s", s.AdminUser, forgejoUser)
	case len(s.AdminPassword) < 8:
		return forgejoSetup{}, errors.New("dev/forgejo: adminPassword must be at least 8 characters")
	case !forgejoEmail.MatchString(s.AdminEmail):
		return forgejoSetup{}, fmt.Errorf("dev/forgejo: adminEmail %q is not an address", s.AdminEmail)
	}
	if !strings.HasSuffix(s.RootURL, "/") {
		s.RootURL += "/"
	}
	u, err := url.Parse(s.RootURL)
	if err != nil {
		return forgejoSetup{}, fmt.Errorf("dev/forgejo: rootURL: %w", err)
	}
	s.Domain = u.Hostname()
	s.Settings, err = forgejoSettingsFrom(cfg["settings"])
	return s, err
}

// forgejoSettingsFrom validates spec.config.settings: app.ini sections of key/value pairs.
func forgejoSettingsFrom(raw any) (map[string]map[string]string, error) {
	if raw == nil {
		return nil, nil
	}
	sections, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("dev/forgejo: settings must map sections to keys")
	}
	out := map[string]map[string]string{}
	for name, rawKeys := range sections {
		keys, ok := rawKeys.(map[string]any)
		if !ok || !forgejoININame.MatchString(name) {
			return nil, fmt.Errorf("dev/forgejo: settings section %q must be a name mapping keys to values", name)
		}
		out[name] = map[string]string{}
		for k, v := range keys {
			val := fmt.Sprint(v)
			if !forgejoININame.MatchString(k) || !forgejoINIVal.MatchString(val) {
				return nil, fmt.Errorf("dev/forgejo: settings %s.%s must be a name and a one-line value", name, k)
			}
			out[name][k] = val
		}
	}
	return out, nil
}

// iniSection is one ordered app.ini section; the empty name is the top level.
type iniSection struct {
	name string
	keys [][2]string
}

func iniSet(secs []iniSection, section, key, val string) []iniSection {
	for i := range secs {
		if secs[i].name != section {
			continue
		}
		for j := range secs[i].keys {
			if secs[i].keys[j][0] == key {
				secs[i].keys[j][1] = val
				return secs
			}
		}
		secs[i].keys = append(secs[i].keys, [2]string{key, val})
		return secs
	}
	return append(secs, iniSection{name: section, keys: [][2]string{{key, val}}})
}

// forgejoINI renders app.ini with every piece of state under WorkPath, then spec.config.settings.
func forgejoINI(s forgejoSetup, runUser string, sec forgejoSecrets) string {
	data := filepath.Join(s.WorkPath, "data")
	var secs []iniSection
	for _, kv := range [][3]string{
		{"", "APP_NAME", "Forgejo"},
		{"", "RUN_MODE", "prod"},
		{"", "RUN_USER", runUser},
		{"", "WORK_PATH", s.WorkPath},
		{"server", "PROTOCOL", "http"},
		{"server", "HTTP_PORT", s.Port},
		{"server", "ROOT_URL", s.RootURL},
		{"server", "DOMAIN", s.Domain},
		{"server", "SSH_DOMAIN", s.Domain},
		{"server", "START_SSH_SERVER", "true"},
		{"server", "SSH_LISTEN_PORT", s.SSHPort},
		{"server", "SSH_PORT", s.PublicSSHPort},
		{"server", "BUILTIN_SSH_SERVER_USER", "git"},
		{"server", "APP_DATA_PATH", data},
		{"server", "LFS_START_SERVER", "true"},
		{"server", "LFS_JWT_SECRET", sec.LFSJWT},
		{"database", "DB_TYPE", "sqlite3"},
		{"database", "PATH", filepath.Join(data, "forgejo.db")},
		{"repository", "ROOT", filepath.Join(s.WorkPath, "repositories")},
		{"security", "INSTALL_LOCK", "true"},
		{"security", "SECRET_KEY", sec.SecretKey},
		{"security", "INTERNAL_TOKEN", sec.InternalToken},
		{"oauth2", "JWT_SECRET", sec.JWT},
		{"service", "DISABLE_REGISTRATION", fmt.Sprint(!s.AllowRegistration)},
		{"session", "PROVIDER", "file"},
		{"log", "MODE", "console"},
		{"log", "LEVEL", "Info"},
		// A failover is a crash: git must fsync every object and ref it acknowledges.
		{"git.config", "core.fsync", "all"},
		{"git.config", "core.fsyncMethod", "fsync"},
	} {
		secs = iniSet(secs, kv[0], kv[1], kv[2])
	}
	for _, name := range sortedKeys(s.Settings) {
		for _, k := range sortedKeys(s.Settings[name]) {
			secs = iniSet(secs, name, k, s.Settings[name][k])
		}
	}
	var b strings.Builder
	for i, sec := range secs {
		if sec.name != "" {
			if i > 0 {
				b.WriteString("\n")
			}
			fmt.Fprintf(&b, "[%s]\n", sec.name)
		}
		for _, kv := range sec.keys {
			fmt.Fprintf(&b, "%s = %s\n", kv[0], kv[1])
		}
	}
	return b.String()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// loadOrCreateForgejoSecrets keeps Forgejo's signing secrets in dir; JWT secrets are base64url.
func loadOrCreateForgejoSecrets(dir string) (forgejoSecrets, error) {
	var out forgejoSecrets
	for _, f := range []struct {
		name string
		dst  *string
		jwt  bool
	}{
		{"secret_key", &out.SecretKey, false},
		{"internal_token", &out.InternalToken, false},
		{"jwt_secret", &out.JWT, true},
		{"lfs_jwt_secret", &out.LFSJWT, true},
	} {
		secret, err := loadOrCreateSecret(filepath.Join(dir, f.name))
		if err != nil {
			return forgejoSecrets{}, err
		}
		*f.dst = secret
		if f.jwt {
			raw, _ := hex.DecodeString(secret)
			*f.dst = base64.RawURLEncoding.EncodeToString(raw)
		}
	}
	return out, nil
}

// ensureForgejoAdmin creates the admin account, or converges its password when it exists.
func ensureForgejoAdmin(s forgejoSetup, run func(args ...string) (string, error)) error {
	out, err := run("admin", "user", "create", "--admin", "--username", s.AdminUser,
		"--password", s.AdminPassword, "--email", s.AdminEmail, "--must-change-password=false")
	if err == nil {
		return nil
	}
	if !strings.Contains(out, "user already exists") {
		return fmt.Errorf("dev/forgejo: creating %s: %w: %s", s.AdminUser, err, strings.TrimSpace(out))
	}
	out, err = run("admin", "user", "change-password", "--username", s.AdminUser,
		"--password", s.AdminPassword, "--must-change-password=false")
	if err != nil {
		return fmt.Errorf("dev/forgejo: setting %s's password: %w: %s", s.AdminUser, err, strings.TrimSpace(out))
	}
	return nil
}

// forgejoEnv gives Forgejo a home: the unit's DynamicUser has none, and Forgejo exits without one.
func forgejoEnv(work string) []string {
	return append(os.Environ(), "HOME="+work)
}

// currentUser names the unit's DynamicUser, which Forgejo checks against RUN_USER.
func currentUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return os.Getenv("USER")
}

// runForgejo serves dev/forgejo from the volume; the SINGLETON strategy keeps one writer.
func runForgejo(ctx context.Context, instance string, args []string) error {
	cfg, err := cfgMap(args)
	if err != nil {
		return err
	}
	mountPath := firstMount(mountPaths(args))
	if mountPath == "" {
		return errors.New("dev/forgejo: no bound storage mount yet")
	}
	if err := waitForMount(mountPath, 30*time.Second, 500*time.Millisecond, realStat); err != nil {
		return fmt.Errorf("dev/forgejo: %w", err)
	}
	s, err := forgejoSetupFrom(mountPath, cfg)
	if err != nil {
		return err
	}
	bin, err := resolveBin("forgejo")
	if err != nil {
		return fmt.Errorf("block runtime: forgejo not found in PATH (ship the package): %w", err)
	}
	if err := mkdirAllRetrying(os.MkdirAll, s.WorkPath, 0o700, 10, 500*time.Millisecond); err != nil {
		return fmt.Errorf("dev/forgejo: %w", err)
	}
	secrets, err := loadOrCreateForgejoSecrets(filepath.Join(s.WorkPath, "secrets"))
	if err != nil {
		return fmt.Errorf("dev/forgejo: secrets: %w", err)
	}
	work := "/tmp/expblk-" + instance
	if err := os.MkdirAll(work, 0o700); err != nil {
		return err
	}
	conf := filepath.Join(work, "app.ini")
	if err := os.WriteFile(conf, []byte(forgejoINI(s, currentUser(), secrets)), 0o600); err != nil {
		return err
	}
	common := []string{"--config", conf, "--work-path", s.WorkPath}
	run := func(argv ...string) (string, error) {
		cmd := exec.CommandContext(ctx, bin, append(argv, common...)...)
		cmd.Env = forgejoEnv(work)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := run("migrate"); err != nil {
		return fmt.Errorf("dev/forgejo: migrate: %w: %s", err, strings.TrimSpace(out))
	}
	if err := ensureForgejoAdmin(s, run); err != nil {
		return err
	}
	// Sessions, avatars and attachments are written without fsync.
	go syncLoop(ctx, mountPath, 2*time.Second, unix.Syncfs)
	cmd := exec.CommandContext(ctx, bin, append([]string{"web"}, common...)...)
	cmd.Env = forgejoEnv(work)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	// A deliberate stop shuts down cleanly; SIGKILL only if that hangs.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = time.Minute
	fmt.Printf("expanse-block-run: forgejo serving on :%s (ssh :%s) as %s\n", s.Port, s.SSHPort, s.RootURL)
	err = cmd.Run()
	if ctx.Err() != nil {
		return nil //nolint:nilerr // deliberate stop
	}
	return err
}
