package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// uptimeKumaPreload makes Uptime Kuma's SQLite sync every commit; it has no setting for that.
//
//go:embed uptimekuma-sync-full.cjs
var uptimeKumaPreload string

// uptimeKumaSetup is everything one monitor/uptime-kuma instance needs.
type uptimeKumaSetup struct {
	DataDir, Port, Scratch, Preload string
}

// uptimeKumaSetupFrom keeps the database on the volume and the preload in the unit's private scratch.
func uptimeKumaSetupFrom(mountPath, scratch string, cfg map[string]any) uptimeKumaSetup {
	return uptimeKumaSetup{
		DataDir: filepath.Join(mountPath, "uptime-kuma"),
		Port:    cfgPortOr(cfg, "3001"),
		Scratch: scratch,
		Preload: filepath.Join(scratch, "sync-full.cjs"),
	}
}

func uptimeKumaEnv(s uptimeKumaSetup) []string {
	return []string{
		"DATA_DIR=" + s.DataDir,
		"HOME=" + s.Scratch,
		"NODE_OPTIONS=--require=" + s.Preload,
		"UPTIME_KUMA_DB_TYPE=sqlite",
		"UPTIME_KUMA_PORT=" + s.Port,
	}
}

func writeUptimeKumaPreload(path string) error {
	return os.WriteFile(path, []byte(uptimeKumaPreload), 0o600)
}

// runUptimeKuma serves monitor/uptime-kuma from the volume; the SINGLETON strategy keeps one writer.
func runUptimeKuma(ctx context.Context, args []string) error {
	cfg, err := cfgMap(args)
	if err != nil {
		return err
	}
	mountPath := firstMount(mountPaths(args))
	if mountPath == "" {
		return errors.New("monitor/uptime-kuma: no bound storage mount yet")
	}
	if err := waitForMount(mountPath, 30*time.Second, 500*time.Millisecond, realStat); err != nil {
		return fmt.Errorf("monitor/uptime-kuma: %w", err)
	}
	s := uptimeKumaSetupFrom(mountPath, filepath.Join(os.TempDir(), "uptime-kuma"), cfg)
	bin, err := resolveBin("uptime-kuma-server")
	if err != nil {
		return fmt.Errorf("block runtime: uptime-kuma-server not found in PATH (ship the package): %w", err)
	}
	for _, dir := range []string{s.DataDir, s.Scratch} {
		if err := mkdirAllRetrying(os.MkdirAll, dir, 0o700, 10, 500*time.Millisecond); err != nil {
			return fmt.Errorf("monitor/uptime-kuma: %w", err)
		}
	}
	if err := writeUptimeKumaPreload(s.Preload); err != nil {
		return fmt.Errorf("monitor/uptime-kuma: %w", err)
	}
	// Uploaded images and db-config.json are written without fsync.
	go syncLoop(ctx, mountPath, 2*time.Second, unix.Syncfs)
	cmd := exec.CommandContext(ctx, bin)
	cmd.Env = append(os.Environ(), uptimeKumaEnv(s)...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = time.Minute
	fmt.Printf("expanse-block-run: uptime-kuma serving on :%s from %s\n", s.Port, s.DataDir)
	err = cmd.Run()
	if ctx.Err() != nil {
		return nil //nolint:nilerr // deliberate stop
	}
	return err
}
