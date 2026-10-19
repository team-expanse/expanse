package vm

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// CloudInit is vm/instance's cloudInit setting, applied by the image's cloud-init on first boot.
type CloudInit struct {
	User     string
	SSHKeys  []string
	Hostname string
}

// CloudInitFromConfig reads config's cloudInit; the zero value when unset.
func CloudInitFromConfig(cfg map[string]any) (CloudInit, error) {
	m, _ := cfg["cloudInit"].(map[string]any)
	str := func(k string) string { s, _ := m[k].(string); return strings.TrimSpace(s) }
	ci := CloudInit{User: str("user"), Hostname: str("hostname")}
	keys, _ := m["sshKeys"].([]any)
	for _, k := range keys {
		s, _ := k.(string)
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		if strings.HasPrefix(s, "gh:") {
			return CloudInit{}, fmt.Errorf("cloudInit.sshKeys: %q is a shortcut the web form resolves; give the public key itself", s)
		}
		ci.SSHKeys = append(ci.SSHKeys, s)
	}
	return ci, nil
}

// SeedFiles renders NoCloud user-data and meta-data. Without a user, the keys go to the
// image's own default user (debian, ubuntu, ...). hostname is used when ci names none.
func SeedFiles(instanceID string, ci CloudInit, hostname string) (userData, metaData []byte) {
	cc := map[string]any{"ssh_pwauth": false}
	if ci.User != "" {
		cc["users"] = []any{"default", map[string]any{
			"name": ci.User, "sudo": "ALL=(ALL) NOPASSWD:ALL", "shell": "/bin/bash",
			"lock_passwd": true, "ssh_authorized_keys": ci.SSHKeys,
		}}
	} else if len(ci.SSHKeys) > 0 {
		cc["ssh_authorized_keys"] = ci.SSHKeys
	}
	if ci.Hostname != "" {
		hostname = ci.Hostname
	}
	ud, _ := yaml.Marshal(cc)
	md, _ := yaml.Marshal(map[string]any{"instance-id": instanceID, "local-hostname": hostname})
	return append([]byte("#cloud-config\n"), ud...), md
}

// BuildSeed writes the NoCloud seed ISO (volume id cidata) into dir with xorriso.
func BuildSeed(ctx context.Context, xorriso, dir, instanceID string, ci CloudInit, hostname string) (string, error) {
	ud, md := SeedFiles(instanceID, ci, hostname)
	files := filepath.Join(dir, "seed")
	if err := os.MkdirAll(files, 0o700); err != nil {
		return "", err
	}
	for name, b := range map[string][]byte{"user-data": ud, "meta-data": md} {
		if err := os.WriteFile(filepath.Join(files, name), b, 0o600); err != nil {
			return "", err
		}
	}
	iso := filepath.Join(dir, "seed.iso")
	out, err := exec.CommandContext(ctx, xorriso, "-as", "mkisofs", "-quiet", "-output", iso,
		"-volid", "cidata", "-joliet", "-rock", files).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("xorriso: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return iso, nil
}
