package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Mirrors nix/blocks/security/vaultwarden/schema.json, so the environment is never built from unchecked text.
var (
	vaultwardenDomain  = regexp.MustCompile(`^https?://([A-Za-z0-9.-]+|\[[0-9A-Fa-f:]+\])(:[0-9]{1,5})?(/[A-Za-z0-9._~/-]*)?$`)
	vaultwardenEnvName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	vaultwardenEnvVal  = regexp.MustCompile(`^[^\n\r]*$`)
)

// vaultwardenSyncInit replaces Vaultwarden's synchronous=NORMAL, under which a crash loses committed WAL frames.
const vaultwardenSyncInit = "PRAGMA busy_timeout = 5000; PRAGMA synchronous = FULL;"

// vaultwardenSetup is everything one security/vaultwarden instance needs.
type vaultwardenSetup struct {
	DataFolder, Port, Domain, AdminToken string
	SignupsAllowed                       bool
	Settings                             map[string]string
}

// vaultwardenSetupFrom applies the block's defaults to spec.config and validates it.
func vaultwardenSetupFrom(mountPath string, cfg map[string]any) (vaultwardenSetup, error) {
	s := vaultwardenSetup{
		DataFolder:     filepath.Join(mountPath, "vaultwarden"),
		Port:           cfgPortOr(cfg, "18000"),
		Domain:         cfgStr(cfg, "domain"),
		AdminToken:     cfgStr(cfg, "adminToken"),
		SignupsAllowed: cfgBool(cfg, "signupsAllowed", false),
	}
	if !vaultwardenDomain.MatchString(s.Domain) {
		return vaultwardenSetup{}, fmt.Errorf("security/vaultwarden: domain %q must be an http(s) URL", s.Domain)
	}
	if !vaultwardenEnvVal.MatchString(s.AdminToken) {
		return vaultwardenSetup{}, errors.New("security/vaultwarden: adminToken must be one line")
	}
	var err error
	s.Settings, err = vaultwardenSettingsFrom(cfg["settings"])
	return s, err
}

// vaultwardenSettingsFrom validates spec.config.settings: Vaultwarden environment variables.
func vaultwardenSettingsFrom(raw any) (map[string]string, error) {
	if raw == nil {
		return nil, nil
	}
	vars, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("security/vaultwarden: settings must map variable names to values")
	}
	out := map[string]string{}
	for k, v := range vars {
		switch v.(type) {
		case string, float64, bool:
		default:
			return nil, fmt.Errorf("security/vaultwarden: settings %s must be a string, number or boolean", k)
		}
		val := fmt.Sprint(v)
		if !vaultwardenEnvName.MatchString(k) || !vaultwardenEnvVal.MatchString(val) {
			return nil, fmt.Errorf("security/vaultwarden: settings %s must be an upper-case name and a one-line value", k)
		}
		out[k] = val
	}
	return out, nil
}

// vaultwardenEnv is the block's own environment, then spec.config.settings over it; webVault "" disables it.
func vaultwardenEnv(s vaultwardenSetup, webVault string) []string {
	vars := map[string]string{
		"DATA_FOLDER":        s.DataFolder,
		"DATABASE_CONN_INIT": vaultwardenSyncInit,
		"ROCKET_ADDRESS":     "0.0.0.0",
		"ROCKET_PORT":        s.Port,
		"DOMAIN":             s.Domain,
		"SIGNUPS_ALLOWED":    fmt.Sprint(s.SignupsAllowed),
		"WEB_VAULT_ENABLED":  fmt.Sprint(webVault != ""),
	}
	if webVault != "" {
		vars["WEB_VAULT_FOLDER"] = webVault
	}
	if s.AdminToken != "" {
		vars["ADMIN_TOKEN"] = s.AdminToken
	}
	for k, v := range s.Settings {
		vars[k] = v
	}
	env := make([]string, 0, len(vars))
	for _, k := range sortedKeys(vars) {
		env = append(env, k+"="+vars[k])
	}
	return env
}

// findWebVault looks for the web vault package's files beside each bin directory on path.
func findWebVault(path string) string {
	for _, dir := range filepath.SplitList(path) {
		vault := filepath.Join(filepath.Dir(dir), "share", "vaultwarden", "vault")
		if st, err := os.Stat(vault); err == nil && st.IsDir() {
			return vault
		}
	}
	return ""
}

// runVaultwarden serves security/vaultwarden from the volume; the SINGLETON strategy keeps one writer.
func runVaultwarden(ctx context.Context, args []string) error {
	cfg, err := cfgMap(args)
	if err != nil {
		return err
	}
	mountPath := firstMount(mountPaths(args))
	if mountPath == "" {
		return errors.New("security/vaultwarden: no bound storage mount yet")
	}
	if err := waitForMount(mountPath, 30*time.Second, 500*time.Millisecond, realStat); err != nil {
		return fmt.Errorf("security/vaultwarden: %w", err)
	}
	s, err := vaultwardenSetupFrom(mountPath, cfg)
	if err != nil {
		return err
	}
	bin, err := resolveBin("vaultwarden")
	if err != nil {
		return fmt.Errorf("block runtime: vaultwarden not found in PATH (ship the package): %w", err)
	}
	if err := mkdirAllRetrying(os.MkdirAll, s.DataFolder, 0o700, 10, 500*time.Millisecond); err != nil {
		return fmt.Errorf("security/vaultwarden: %w", err)
	}
	webVault := findWebVault(os.Getenv("PATH"))
	if webVault == "" {
		fmt.Println("expanse-block-run: no vaultwarden web vault beside PATH; serving the API only")
	}
	// Attachments, sends and config.json are written without fsync.
	go syncLoop(ctx, mountPath, 2*time.Second, unix.Syncfs)
	cmd := exec.CommandContext(ctx, bin)
	cmd.Env = append(os.Environ(), vaultwardenEnv(s, webVault)...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = time.Minute
	fmt.Printf("expanse-block-run: vaultwarden serving on :%s as %s\n", s.Port, s.Domain)
	err = cmd.Run()
	if ctx.Err() != nil {
		return nil //nolint:nilerr // deliberate stop
	}
	return err
}
