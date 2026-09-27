package install

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Stage is one step of the install.
type Stage struct {
	Name   string
	Desc   string
	Run    func(ctx *RunContext) error
	DryRun string // command(s) that would run, for --dry-run output
}

// RunContext carries state between stages.
type RunContext struct {
	Config            *Config
	Disks             []Disk
	Selected          []Disk
	Layout            DiskLayout
	Force             bool
	TargetFlake       string
	Mount             string // /mnt
	SkipSystemInstall bool
	Identity          *Identity
	Logger            func(format string, args ...any)
	Exec              func(name string, args ...string) error // overridable for tests
	Out               io.Writer                               // commands' stdout and stderr; nil = os.Stdout/os.Stderr
}

func (rc *RunContext) run(name string, args ...string) error {
	if rc.Exec != nil {
		return rc.Exec(name, args...)
	}
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if rc.Out != nil {
		cmd.Stdout, cmd.Stderr = rc.Out, rc.Out
	}
	return cmd.Run()
}

// Options configure an install run.
type Options struct {
	ConfigPath  string
	DryRun      bool
	Force       bool
	TargetFlake string
	Logger      func(format string, args ...any)
	Out         io.Writer // commands' output; nil = os.Stdout/os.Stderr
	// SkipSystemInstall runs every stage except nixos-install (used by the
	// VM test: partition, snapshot, identity, config; system evaluation
	// is exercised by the two-VM ISO harness instead).
	SkipSystemInstall bool
}

// DefaultTargetFlake is the flake baked into the installer ISO.
const DefaultTargetFlake = "/run/expanse-flake"

// Stages returns the ordered install stages.
func Stages() []Stage {
	return []Stage{
		{
			Name: "preflight", Desc: "check hardware requirements (arch, RAM >= 2G, disk >= 20G, network)", Run: stagePreflight,
			DryRun: "lsblk -J; free -m",
		},
		{
			Name: "detect", Desc: "detect target disks", Run: stageDetect,
			DryRun: "lsblk -J -p -o NAME,PATH,SIZE,MODEL,TYPE,RM,MOUNTPOINT,FSTYPE",
		},
		{
			Name: "confirm", Desc: "confirm the destructive plan (WILL WIPE selected disks)", Run: stageConfirm,
			DryRun: "interactive confirmation (or --force)",
		},
		{
			Name: "partition", Desc: "partition + format via disko", Run: stagePartition,
			DryRun: "disko --mode destroy,format,mount <layout>.nix --arg disks [...]",
		},
		{
			Name: "snapshot", Desc: "take the @root-blank snapshot (impermanence anchor)", Run: stageSnapshot,
			DryRun: "mount -o subvolid=5 <system-partition> <tmp>; btrfs subvolume snapshot -r <tmp>/@root <tmp>/@root-blank",
		},
		{
			Name: "identity", Desc: "generate node identity (UUID + Ed25519 keypair)", Run: stageIdentity,
			DryRun: "EnsureIdentity(/mnt/persist/expanse/identity)",
		},
		{
			Name: "config", Desc: "generate system configuration under /mnt", Run: stageConfig,
			DryRun: "write /mnt/persist/expanse/install-config.yaml; nixos-generate-config --root /mnt --no-filesystems --dir /mnt/persist/etc/nixos; write /mnt/persist/etc/nixos/configuration.nix",
		},
		{
			Name: "install", Desc: "nixos-install onto /mnt", Run: stageInstall,
			DryRun: "env NIXOS_CONFIG=/mnt/persist/etc/nixos/configuration.nix nixos-install --root /mnt --no-root-password",
		},
		{
			Name: "verify", Desc: "verify bootloader, subvolumes, blank snapshot, identity", Run: stageVerify,
			DryRun: "btrfs subvolume list <mount>; btrfs subvolume list <mount> | grep @root-blank",
		},
	}
}

// Run executes the install plan. On dryRun it prints each stage and the
// commands it would run, touching nothing.
func Run(opts Options) error {
	logf := opts.Logger
	if logf == nil {
		logf = func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }
	}

	rc := &RunContext{TargetFlake: opts.TargetFlake, Logger: logf, Mount: "/mnt", Out: opts.Out}
	if rc.TargetFlake == "" {
		rc.TargetFlake = DefaultTargetFlake
	}

	if opts.ConfigPath == "" {
		return fmt.Errorf("no config provided (use --config or the interactive installer)")
	}
	cfg, err := LoadConfig(opts.ConfigPath)
	if err != nil {
		return err
	}
	rc.Config = cfg
	rc.Force = opts.Force || cfg.Disks.Force
	rc.SkipSystemInstall = opts.SkipSystemInstall

	for _, st := range Stages() {
		if opts.DryRun {
			cmds := st.DryRun
			if cmds == "" {
				cmds = st.Desc
			}
			logf("[dry-run] %-10s %s", st.Name+":", strings.ReplaceAll(cmds, "\n", "\n[dry-run]           "))
			continue
		}
		start := time.Now()
		logf("==> %s: %s", st.Name, st.Desc)
		if err := st.Run(rc); err != nil {
			return fmt.Errorf("stage %s: %w", st.Name, err)
		}
		logf("<== %s done (%.1fs)", st.Name, time.Since(start).Seconds())
	}

	if !opts.DryRun {
		report(rc)
	}
	return nil
}

func stagePreflight(rc *RunContext) error {
	disks, err := DetectDisks()
	if err != nil {
		return err
	}
	pf, err := RunPreflight(disks)
	if err != nil {
		return err
	}
	for _, w := range pf.Warnings {
		rc.Logger("warning: %s", w)
	}
	return nil
}

func stageDetect(rc *RunContext) error {
	disks, err := DetectDisks()
	if err != nil {
		return err
	}
	rc.Disks = disks
	return nil
}

func stageConfirm(rc *RunContext) error {
	// The force override must have been approved by the caller (flag or TUI
	// typed confirmation). Layout validation and disk selection happen here.
	selected, layout, err := SelectDisks(rc.Config, rc.Disks, rc.Force)
	if err != nil {
		return err
	}
	rc.Selected = selected
	rc.Layout = layout
	return nil
}

func stagePartition(rc *RunContext) error {
	// Build the disko invocation for the chosen layout. disko takes the
	// layout file with the disk list passed via --arg disks.
	return rc.runDisko(rc.Selected)
}

func (rc *RunContext) diskoCommand(disks []Disk) (string, []string) {
	layoutFile := fmt.Sprintf("%s/nix/installer/disko/%s.nix", rc.TargetFlake, rc.Layout)
	devices := make([]string, len(disks))
	for i, d := range disks {
		devices[i] = d.Path
	}
	return "disko", []string{"--mode", "destroy,format,mount", "--yes-wipe-all-disks", "--arg", "disks", "[" + strings.Join(quoted(devices), " ") + "]", layoutFile}
}

func (rc *RunContext) runDisko(disks []Disk) error {
	name, args := rc.diskoCommand(disks)
	return rc.run(name, args...)
}

// systemDevice returns the by-partlabel path of the layout's btrfs system
// partition -- the side disko actually formats, which for a mirror is
// "system-b" (see nix/installer/disko/{single,mirror}.nix).
func systemDevice(layout DiskLayout) string {
	disk := "system"
	if layout == LayoutMirror {
		disk = "system-b"
	}
	return "/dev/disk/by-partlabel/disk-" + disk + "-root"
}

// blankSnapshot is impermanence's rollback target: a top-level sibling of
// @root, not nested inside it, so wiping @root never takes it down too.
const blankSnapshot = "@root-blank"

func stageSnapshot(rc *RunContext) error {
	// Must run before anything is written to /mnt: the blank snapshot is
	// what impermanence rolls back to. disko only mounts individual
	// subvolumes, so reach the top-level (subvolid=5) with a throwaway
	// mount to see @root and its future sibling at once.
	top, err := os.MkdirTemp("", "expanse-btrfs-top")
	if err != nil {
		return err
	}
	defer os.RemoveAll(top)
	dev := systemDevice(rc.Layout)
	if err := rc.run("mount", "-o", "subvolid=5", dev, top); err != nil {
		return err
	}
	defer rc.run("umount", top)
	return rc.run("btrfs", "subvolume", "snapshot", "-r", filepath.Join(top, "@root"), filepath.Join(top, blankSnapshot))
}

// nixosConfigDir holds the node's NixOS config under /persist; impermanence binds it to /etc/nixos.
const nixosConfigDir = "persist/etc/nixos"

func stageConfig(rc *RunContext) error {
	dir := filepath.Join(rc.Mount, nixosConfigDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	if err := rc.Config.WriteYAML(filepath.Join(rc.Mount, "persist/expanse/install-config.yaml")); err != nil {
		return err
	}
	// hardware-configuration.nix: the initrd's drivers for this machine's disks; disko owns the mounts.
	if err := rc.run("nixos-generate-config", "--root", rc.Mount, "--no-filesystems", "--dir", dir); err != nil {
		return err
	}
	return writeConfiguration(rc, filepath.Join(dir, "configuration.nix"))
}

func stageInstall(rc *RunContext) error {
	if rc.SkipSystemInstall {
		rc.Logger("skipping nixos-install (testing)")
		return nil
	}
	config := "NIXOS_CONFIG=" + filepath.Join(rc.Mount, nixosConfigDir, "configuration.nix")
	return rc.run("env", config, "nixos-install", "--root", rc.Mount, "--no-root-password")
}

func stageIdentity(rc *RunContext) error {
	// /mnt/persist is mounted by disko at this point.
	id, err := EnsureIdentity(filepath.Join(rc.Mount, "persist/expanse/identity"))
	if err != nil {
		return err
	}
	rc.Identity = id
	return nil
}

func stageVerify(rc *RunContext) error {
	checks := []struct{ name, cmd string }{
		{"subvolumes", "test -d " + rc.Mount + "/persist"},
		{"blank snapshot", "btrfs subvolume list " + rc.Mount + " | grep -q " + blankSnapshot},
		{"identity", "test -s " + rc.Mount + "/persist/expanse/identity/node-id"},
	}
	if !rc.SkipSystemInstall {
		checks = append(checks,
			struct{ name, cmd string }{"bootloader", "test -e " + rc.Mount + "/boot/EFI"})
	}
	for _, c := range checks {
		if err := rc.run("sh", "-c", c.cmd); err != nil {
			return fmt.Errorf("verify %s: %w", c.name, err)
		}
	}
	return nil
}

// writeConfiguration generates the target's configuration.nix, which
// imports the expanse module set from the flake baked into the ISO.
// hostId and hostname are derived from the identity (created in the
// identity stage, before this one).
func writeConfiguration(rc *RunContext, path string) error {
	hostID := "00000000"
	hostname := rc.Config.Hostname
	if rc.Identity != nil {
		hostID = rc.Identity.HostID()
		if hostname == "" {
			hostname = rc.Identity.DefaultHostname()
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "{ config, pkgs, ... }:\n")
	fmt.Fprintf(&b, "{\n")
	fmt.Fprintf(&b, "  # Partitioning was done by disko at install time; the same layout declares the mounts.\n")
	fmt.Fprintf(&b, "  imports = [\n")
	fmt.Fprintf(&b, "    ./hardware-configuration.nix\n")
	fmt.Fprintf(&b, "    %s/nix/modules/expanse-node.nix\n", rc.TargetFlake)
	fmt.Fprintf(&b, "    (import %s/nix/modules/disk-layout.nix {\n", rc.TargetFlake)
	fmt.Fprintf(&b, "      layout = %s/nix/installer/disko/%s.nix;\n", rc.TargetFlake, rc.Layout)
	fmt.Fprintf(&b, "      disks = [ %s ];\n", strings.Join(quoted(rc.Config.Disks.Devices), " "))
	fmt.Fprintf(&b, "    })\n")
	fmt.Fprintf(&b, "  ];\n")
	fmt.Fprintf(&b, "  expanse.hostId = %q;\n", hostID)
	fmt.Fprintf(&b, "  expanse.hostname = %q;\n", hostname)
	fmt.Fprintf(&b, "  expanse.ssh.authorizedKeys = [ %s ];\n", strings.Join(quoted(rc.Config.SSH.AuthorizedKeys), " "))
	fmt.Fprintf(&b, "  expanse.disks = [ %s ];\n", strings.Join(quoted(rc.Config.Disks.Devices), " "))
	fmt.Fprintf(&b, "}\n")
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// report prints the post-install summary.
func report(rc *RunContext) {
	id := ""
	if rc.Identity != nil {
		id = rc.Identity.NodeID.String()
	}
	fmt.Printf("\nExpanse node installed.\n")
	fmt.Printf("  node ID: %s\n", id)
	fmt.Printf("  reboot and the node will come up at http://<node-ip>:8443\n")
	fmt.Printf("  next: reboot, then `expanse agent status`\n")
}

func quoted(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = fmt.Sprintf("%q", s)
	}
	return out
}
