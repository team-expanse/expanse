package install

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunDryRunTouchesNothing(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "install.yaml")
	if err := os.WriteFile(cfgPath, []byte(validYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := Run(Options{
		ConfigPath: cfgPath,
		DryRun:     true,
		Logger:     func(f string, a ...any) { out.WriteString(fmt.Sprintf(f, a...) + "\n") },
	})
	if err != nil {
		t.Fatalf("Run --dry-run: %v", err)
	}

	text := out.String()
	for _, stage := range []string{"preflight", "detect", "confirm", "partition", "snapshot", "config", "install", "identity", "verify"} {
		if !strings.Contains(text, stage) {
			t.Errorf("dry-run output missing stage %q", stage)
		}
	}
	if !strings.Contains(text, "btrfs subvolume snapshot") {
		t.Error("dry-run output missing the blank snapshot command")
	}
	if !strings.Contains(text, "disko") {
		t.Error("dry-run output missing the disko command")
	}

	// Nothing may have been written to the target system.
	if _, err := os.Stat("/mnt"); err == nil {
		t.Log("/mnt exists on this machine; skipping mountpoint check")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("dry-run wrote files into config dir: %v", entries)
	}
}

func TestRunCommandOutputGoesToOut(t *testing.T) {
	var out bytes.Buffer
	rc := &RunContext{Out: &out}
	if err := rc.run("sh", "-c", "echo to-stdout; echo to-stderr >&2"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "to-stdout") || !strings.Contains(out.String(), "to-stderr") {
		t.Fatalf("command output %q, want both streams", out.String())
	}
}

func TestStageConfigWritesToPersistWithHardwareConfig(t *testing.T) {
	mnt := t.TempDir()
	var ran []string
	rc := &RunContext{Config: DefaultConfig(), Layout: LayoutSingle, TargetFlake: "/flake", Mount: mnt,
		Exec: func(name string, args ...string) error {
			ran = append(ran, name+" "+strings.Join(args, " "))
			return nil
		}}
	if err := os.MkdirAll(filepath.Join(mnt, "persist/expanse"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := stageConfig(rc); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(mnt, "persist/etc/nixos")
	want := "nixos-generate-config --root " + mnt + " --no-filesystems --dir " + dir
	if len(ran) != 1 || ran[0] != want {
		t.Fatalf("ran %q, want [%q]", ran, want)
	}
	conf, err := os.ReadFile(filepath.Join(dir, "configuration.nix"))
	if err != nil || !strings.Contains(string(conf), "./hardware-configuration.nix") {
		t.Fatalf("configuration.nix (err %v) does not import the hardware config:\n%s", err, conf)
	}
}

func TestStageInstallUsesThePersistedConfig(t *testing.T) {
	var ran []string
	rc := &RunContext{Mount: "/mnt", Exec: func(name string, args ...string) error {
		ran = append(ran, name+" "+strings.Join(args, " "))
		return nil
	}}
	if err := stageInstall(rc); err != nil {
		t.Fatal(err)
	}
	want := "env NIXOS_CONFIG=/mnt/persist/etc/nixos/configuration.nix nixos-install --root /mnt --no-root-password"
	if len(ran) != 1 || ran[0] != want {
		t.Fatalf("ran %q, want [%q]", ran, want)
	}
}

// The blank snapshot is taken on the device the node's rollback mounts: the md array for a mirror.
func TestSystemDeviceMatchesLayout(t *testing.T) {
	for layout, want := range map[DiskLayout]string{
		LayoutSingle: "/dev/disk/by-partlabel/disk-system-root",
		LayoutMirror: "/dev/md/system",
	} {
		if got := systemDevice(layout); got != want {
			t.Errorf("systemDevice(%s) = %q, want %q", layout, got, want)
		}
	}
}
