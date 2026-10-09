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

// Mirrors nix/blocks/media/jellyfin/schema.json, so the command line is never built from unchecked text.
var jellyfinURL = regexp.MustCompile(`^https?://([A-Za-z0-9.-]+|\[[0-9A-Fa-f:]+\])(:[0-9]{1,5})?(/[A-Za-z0-9._~/-]*)?$`)

// jellyfinDatabaseXML picks SQLite with synchronous=FULL; Jellyfin's default NORMAL loses committed WAL frames in a crash.
const jellyfinDatabaseXML = `<?xml version="1.0" encoding="utf-8"?>
<DatabaseConfigurationOptions xmlns:xsd="http://www.w3.org/2001/XMLSchema" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">
  <DatabaseType>Jellyfin-SQLite</DatabaseType>
  <CustomProviderOptions>
    <PluginName />
    <PluginAssembly />
    <ConnectionString />
    <Options>
      <CustomDatabaseOption>
        <Key>syncmode</Key>
        <Value>2</Value>
      </CustomDatabaseOption>
    </Options>
  </CustomProviderOptions>
  <LockingBehavior>NoLock</LockingBehavior>
</DatabaseConfigurationOptions>
`

// jellyfinSetup is everything one media/jellyfin instance needs.
type jellyfinSetup struct {
	DataDir, ConfigDir, CacheDir, LogDir, PublishedURL string
}

// jellyfinSetupFrom keeps the library database and settings on the volume, and the rebuildable cache and transcodes off it.
func jellyfinSetupFrom(mountPath, scratch string, cfg map[string]any) (jellyfinSetup, error) {
	s := jellyfinSetup{
		DataDir:      filepath.Join(mountPath, "jellyfin", "data"),
		ConfigDir:    filepath.Join(mountPath, "jellyfin", "config"),
		CacheDir:     filepath.Join(scratch, "cache"),
		LogDir:       filepath.Join(scratch, "log"),
		PublishedURL: cfgStr(cfg, "publishedServerUrl"),
	}
	if s.PublishedURL != "" && !jellyfinURL.MatchString(s.PublishedURL) {
		return jellyfinSetup{}, fmt.Errorf("media/jellyfin: publishedServerUrl %q must be an http(s) URL", s.PublishedURL)
	}
	return s, nil
}

func jellyfinArgs(s jellyfinSetup) []string {
	args := []string{
		"--datadir", s.DataDir, "--configdir", s.ConfigDir,
		"--cachedir", s.CacheDir, "--logdir", s.LogDir,
		// Watching for network changes needs AF_NETLINK, which the unit's sandbox withholds.
		"--nonetchange",
	}
	if s.PublishedURL != "" {
		args = append(args, "--published-server-url", s.PublishedURL)
	}
	return args
}

// writeJellyfinDatabaseConfig replaces database.xml on every start, so the durable sync mode cannot drift.
func writeJellyfinDatabaseConfig(configDir string) error {
	return writeFileDurably(filepath.Join(configDir, "database.xml"), []byte(jellyfinDatabaseXML), 0o600)
}

// runJellyfin serves media/jellyfin from the volume; the SINGLETON strategy keeps one writer.
func runJellyfin(ctx context.Context, args []string) error {
	cfg, err := cfgMap(args)
	if err != nil {
		return err
	}
	mountPath := firstMount(mountPaths(args))
	if mountPath == "" {
		return errors.New("media/jellyfin: no bound storage mount yet")
	}
	if err := waitForMount(mountPath, 30*time.Second, 500*time.Millisecond, realStat); err != nil {
		return fmt.Errorf("media/jellyfin: %w", err)
	}
	// The unit's private /tmp: transcodes and image cache stay off the replicated volume.
	scratch := filepath.Join(os.TempDir(), "jellyfin")
	s, err := jellyfinSetupFrom(mountPath, scratch, cfg)
	if err != nil {
		return err
	}
	bin, err := resolveBin("jellyfin")
	if err != nil {
		return fmt.Errorf("block runtime: jellyfin not found in PATH (ship the package): %w", err)
	}
	for _, dir := range []string{s.DataDir, s.ConfigDir, s.CacheDir, s.LogDir} {
		if err := mkdirAllRetrying(os.MkdirAll, dir, 0o700, 10, 500*time.Millisecond); err != nil {
			return fmt.Errorf("media/jellyfin: %w", err)
		}
	}
	if err := writeJellyfinDatabaseConfig(s.ConfigDir); err != nil {
		return fmt.Errorf("media/jellyfin: %w", err)
	}
	// Library settings, images and metadata are written without fsync.
	go syncLoop(ctx, mountPath, 2*time.Second, unix.Syncfs)
	cmd := exec.CommandContext(ctx, bin, jellyfinArgs(s)...)
	cmd.Env = append(os.Environ(), "HOME="+scratch)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = time.Minute
	fmt.Printf("expanse-block-run: jellyfin serving on :8096 from %s\n", filepath.Dir(s.DataDir))
	err = cmd.Run()
	if ctx.Err() != nil {
		return nil //nolint:nilerr // deliberate stop
	}
	return err
}
