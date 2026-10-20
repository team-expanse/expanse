package vm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCloudInitFromConfig(t *testing.T) {
	ci, err := CloudInitFromConfig(map[string]any{"cloudInit": map[string]any{
		"user": "ops", "hostname": "box", "sshKeys": []any{"ssh-ed25519 AAAA a@b", "  "},
	}})
	if err != nil || ci.User != "ops" || ci.Hostname != "box" || len(ci.SSHKeys) != 1 {
		t.Fatalf("%+v, %v", ci, err)
	}
	if _, err := CloudInitFromConfig(map[string]any{"cloudInit": map[string]any{"sshKeys": []any{"gh:someone"}}}); err == nil {
		t.Error("a gh: shortcut reached the runtime; the web form resolves it to keys")
	}
}

func TestCloudInitFromConfigReadsTheSetup(t *testing.T) {
	ci, err := CloudInitFromConfig(map[string]any{"cloudInit": map[string]any{"setup": map[string]any{
		"packages": []any{"docker.io", " "}, "runcmd": []any{"echo hi", ""},
	}}})
	if err != nil || len(ci.Packages) != 1 || ci.Packages[0] != "docker.io" || len(ci.RunCmd) != 1 || ci.RunCmd[0] != "echo hi" {
		t.Fatalf("%+v, %v", ci, err)
	}
}

func cloudConfig(t *testing.T, ud []byte) map[string]any {
	t.Helper()
	if !strings.HasPrefix(string(ud), "#cloud-config\n") {
		t.Fatalf("user-data does not start with #cloud-config:\n%s", ud)
	}
	var m map[string]any
	if err := yaml.Unmarshal(ud, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestSeedNamesAUserWithTheKeys(t *testing.T) {
	ud, md := SeedFiles("default-vm1-0", CloudInit{User: "ops", SSHKeys: []string{"ssh-ed25519 AAAA k1"}}, "vm1")
	users := cloudConfig(t, ud)["users"].([]any)
	if users[0] != "default" {
		t.Errorf("users[0] = %v, want the image's default user kept", users[0])
	}
	u := users[1].(map[string]any)
	if u["name"] != "ops" || u["ssh_authorized_keys"].([]any)[0] != "ssh-ed25519 AAAA k1" || u["sudo"] == nil {
		t.Errorf("user = %v", u)
	}
	var meta map[string]any
	if err := yaml.Unmarshal(md, &meta); err != nil {
		t.Fatal(err)
	}
	if meta["instance-id"] != "default-vm1-0" || meta["local-hostname"] != "vm1" {
		t.Errorf("meta-data = %v", meta)
	}
}

func TestSeedWithoutAUserGivesTheKeysToTheImagesDefaultUser(t *testing.T) {
	ud, md := SeedFiles("i", CloudInit{SSHKeys: []string{"k1", "k2"}, Hostname: "named"}, "vm1")
	cc := cloudConfig(t, ud)
	if _, ok := cc["users"]; ok {
		t.Error("users set without a user name")
	}
	if keys := cc["ssh_authorized_keys"].([]any); len(keys) != 2 {
		t.Errorf("keys = %v", keys)
	}
	if !strings.Contains(string(md), "local-hostname: named") {
		t.Errorf("meta-data = %s", md)
	}
}

func TestSeedInstallsPackagesThenRunsCommands(t *testing.T) {
	ud, _ := SeedFiles("i", CloudInit{Packages: []string{"docker.io"}, RunCmd: []string{"echo a", "echo b"}}, "vm1")
	cc := cloudConfig(t, ud)
	if cc["package_update"] != true || cc["packages"].([]any)[0] != "docker.io" {
		t.Errorf("packages without a package index refresh: %v", cc)
	}
	if cmds := cc["runcmd"].([]any); len(cmds) != 2 || cmds[1] != "echo b" {
		t.Errorf("runcmd = %v", cmds)
	}
}

func TestSeedWithoutASetupLeavesPackagesAndCommandsOut(t *testing.T) {
	cc := cloudConfig(t, func() []byte { ud, _ := SeedFiles("i", CloudInit{}, "vm1"); return ud }())
	for _, k := range []string{"packages", "package_update", "runcmd"} {
		if _, ok := cc[k]; ok {
			t.Errorf("%s set without a setup", k)
		}
	}
}

func TestBuildSeedMakesACidataISO(t *testing.T) {
	xorriso, err := exec.LookPath("xorriso")
	if err != nil {
		t.Skip("no xorriso")
	}
	dir := t.TempDir()
	iso, err := BuildSeed(context.Background(), xorriso, dir, "i", CloudInit{}, "vm1")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(iso)
	if err != nil || filepath.Dir(iso) != dir {
		t.Fatal(iso, err)
	}
	// The primary volume descriptor's volume id, at 32 KiB + 40: cloud-init finds the seed by it.
	if id := strings.TrimSpace(string(raw[32768+40 : 32768+72])); !strings.EqualFold(id, "cidata") {
		t.Errorf("volume id %q", id)
	}
}
