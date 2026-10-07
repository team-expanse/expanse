package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// nfsSquash maps the schema's squash values to Ganesha's export option.
var nfsSquash = map[string]string{"root": "Root_Squash", "none": "No_Root_Squash", "all": "All_Squash"}

// nfsSetup is everything one share/nfs instance's ganesha.conf needs.
type nfsSetup struct {
	Port, Pseudo, SharePath, RecoveryRoot, PluginsDir string
	ReadOnly                                          bool
	Squash                                            string
	Grace, Lease                                      int
	Fsid                                              string
}

// nfsSetupFrom applies the block's defaults to spec.config and validates it.
func nfsSetupFrom(instance, mountPath, pluginsDir string, cfg map[string]any) (nfsSetup, error) {
	squashKey := cfgStr(cfg, "squash")
	if squashKey == "" {
		squashKey = "root"
	}
	squash, ok := nfsSquash[squashKey]
	if !ok {
		return nfsSetup{}, fmt.Errorf("share/nfs: squash %q is not root, none or all", squashKey)
	}
	pseudo := cfgStr(cfg, "pseudo")
	if pseudo == "" {
		pseudo = "/share"
	}
	if !strings.HasPrefix(pseudo, "/") {
		return nfsSetup{}, fmt.Errorf("share/nfs: pseudo %q must be absolute", pseudo)
	}
	rel := cfgStr(cfg, "path")
	if rel == "" {
		rel = "share"
	}
	// The volume root also holds .nfs-state, which clients must never see.
	if rel = filepath.Clean(rel); rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || filepath.IsAbs(rel) {
		return nfsSetup{}, fmt.Errorf("share/nfs: path %q must be a directory inside the volume", rel)
	}
	grace := cfgInt(cfg, "gracePeriod", 90)
	return nfsSetup{
		Port:         cfgPortOr(cfg, "12049"),
		Pseudo:       pseudo,
		SharePath:    filepath.Join(mountPath, rel),
		RecoveryRoot: filepath.Join(mountPath, ".nfs-state", "recovery"),
		PluginsDir:   pluginsDir,
		ReadOnly:     cfgBool(cfg, "readOnly", false),
		Squash:       squash,
		Grace:        grace,
		Lease:        min(60, grace), // Ganesha warns when grace is shorter than a lease
		Fsid:         nfsFsid(instance),
	}, nil
}

// nfsFsid pins the export's fsid per instance so clients see the same
// filesystem after failover (PHASE-03-TASKS.md D6).
func nfsFsid(instance string) string {
	sum := sha256.Sum256([]byte("expanse-nfs-fsid:" + instance))
	return fmt.Sprintf("%d.%d", binary.BigEndian.Uint32(sum[:4]), binary.BigEndian.Uint32(sum[4:8]))
}

// ganeshaPluginsDir is the FSAL directory of the package holding bin.
func ganeshaPluginsDir(bin string) string {
	return filepath.Join(filepath.Dir(filepath.Dir(bin)), "lib", "ganesha")
}

// ganeshaConfig renders an NFSv4-only, single-export ganesha.conf.
func ganeshaConfig(s nfsSetup) string {
	access := "RW"
	if s.ReadOnly {
		access = "RO"
	}
	return fmt.Sprintf(`NFS_CORE_PARAM {
  NFS_Port = %s;
  Protocols = 4;
  Enable_NLM = false;
  Enable_RQUOTA = false;
  Enable_UDP = false;
  Enable_Metrics = false;
  Plugins_Dir = %q;
}
NFSV4 {
  RecoveryBackend = fs;
  RecoveryRoot = %q;
  Grace_Period = %d;
  Lease_Lifetime = %d;
  Minor_Versions = 1, 2;
  Allow_Numeric_Owners = true;
  Only_Numeric_Owners = true;
}
EXPORT {
  Export_Id = 1;
  Path = %q;
  Pseudo = %q;
  Access_Type = %s;
  Squash = %s;
  # Squashed users become the system's nobody, not Ganesha's default -2.
  Anonymous_Uid = 65534;
  Anonymous_Gid = 65534;
  SecType = sys;
  Protocols = 4;
  Transports = TCP;
  Filesystem_Id = %s;
  FSAL { Name = VFS; }
}
`, s.Port, s.PluginsDir, s.RecoveryRoot, s.Grace, s.Lease, s.SharePath, s.Pseudo, access, s.Squash, s.Fsid)
}

// runNFS serves share/nfs with userspace NFS-Ganesha: one process per
// instance, its client-recovery state on the bound volume (D3).
func runNFS(ctx context.Context, instance string, args []string) error {
	cfg, err := cfgMap(args)
	if err != nil {
		return err
	}
	mountPath := firstMount(mountPaths(args))
	if mountPath == "" {
		return fmt.Errorf("share/nfs: no bound storage mount yet")
	}
	if err := waitForMount(mountPath, 30*time.Second, 500*time.Millisecond, realStat); err != nil {
		return fmt.Errorf("share/nfs: %w", err)
	}
	bin, err := resolveBin("ganesha.nfsd")
	if err != nil {
		return fmt.Errorf("block runtime: ganesha.nfsd not found in PATH (ship the package): %w", err)
	}
	s, err := nfsSetupFrom(instance, mountPath, ganeshaPluginsDir(bin), cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.RecoveryRoot, 0o700); err != nil {
		return err
	}
	// Created world-writable once so squashed clients can write; later chmods are the operator's.
	if _, err := os.Stat(s.SharePath); os.IsNotExist(err) {
		if err := os.MkdirAll(s.SharePath, 0o755); err != nil {
			return err
		}
		if err := os.Chmod(s.SharePath, 0o1777); err != nil {
			return err
		}
	}
	work := "/tmp/expblk-" + instance
	if err := os.MkdirAll(work, 0o700); err != nil {
		return err
	}
	conf := filepath.Join(work, "ganesha.conf")
	if err := os.WriteFile(conf, []byte(ganeshaConfig(s)), 0o600); err != nil {
		return err
	}
	fmt.Printf("expanse-block-run: nfs exporting %s as %s on :%s\n", s.SharePath, s.Pseudo, s.Port)
	argv := []string{"-F", "-L", "STDOUT", "-N", "NIV_EVENT", "-f", conf, "-p", filepath.Join(work, "ganesha.pid")}
	return execWorkload(ctx, bin, argv)
}
