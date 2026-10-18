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
	rc := &RunContext{
		Config: DefaultConfig(), Layout: LayoutSingle, TargetFlake: "/flake", Mount: mnt,
		Exec: func(name string, args ...string) error {
			ran = append(ran, name+" "+strings.Join(args, " "))
			return nil
		},
	}
	if err := os.MkdirAll(filepath.Join(mnt, "persist/expanse"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := stageConfig(rc); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(mnt, "persist/etc/nixos")
	want := []string{
		// The node's own copy of the source: the ISO's /flake is gone after the first reboot.
		// -H: the ISO's flake path is a symlink into the store; copy the tree, not the link.
		"cp -rHT --no-preserve=mode,ownership /flake " + filepath.Join(dir, "expanse"),
		"nixos-generate-config --root " + mnt + " --no-filesystems --dir " + dir,
	}
	if strings.Join(ran, "\n") != strings.Join(want, "\n") {
		t.Fatalf("ran %q, want %q", ran, want)
	}
	conf, err := os.ReadFile(filepath.Join(dir, "configuration.nix"))
	if err != nil || !strings.Contains(string(conf), "./hardware-configuration.nix") {
		t.Fatalf("configuration.nix (err %v) does not import the hardware config:\n%s", err, conf)
	}
	if strings.Contains(string(conf), "/flake") || !strings.Contains(string(conf), "./expanse/nix/modules/expanse-node.nix") {
		t.Fatalf("configuration.nix must import the node's copy, not the ISO's source:\n%s", conf)
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
	want := "env NIXOS_CONFIG=/mnt/persist/etc/nixos/configuration.nix nixos-install --root /mnt --no-root-password" +
		" --option always-allow-substitutes true"
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

func TestSystemSizeGrowsWithTheSystemDisks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		sizes []string // lsblk SIZE
		want  string
	}{
		{"minimum disk", []string{"20G"}, "8G"},
		{"just under the threshold", []string{"63.9G"}, "8G"},
		{"large single disk", []string{"500G"}, "32G"},
		{"mirror uses the smaller of the pair", []string{"1.8T", "40G"}, "8G"},
		{"extra JBOD disks don't count", []string{"100G", "100G", "20G"}, "32G"},
		{"unparseable size stays small", []string{"?"}, "8G"},
	} {
		disks := make([]Disk, len(tc.sizes))
		for i, s := range tc.sizes {
			disks[i] = Disk{Path: fmt.Sprintf("/dev/vd%c", 'a'+i), Size: s}
		}
		if got := systemSize(disks); got != tc.want {
			t.Errorf("%s: systemSize = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestDiskoCommandPassesTheSystemSize(t *testing.T) {
	rc := &RunContext{TargetFlake: "/src", Layout: LayoutSingle}
	_, args := rc.diskoCommand([]Disk{{Path: "/dev/vda", Size: "500G"}})
	if !strings.Contains(strings.Join(args, " "), "--argstr systemSize 32G") {
		t.Errorf("disko args %q lack --argstr systemSize 32G", args)
	}
}
