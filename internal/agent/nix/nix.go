// Package nix implements the agent's nix driver: building flake attributes,
// switching system configurations, generations, rollback, and GC — plus the
// pending-switch watchdog that makes remote config changes survivable (D2.6).
package nix

import (
	"bufio"
	"bytes"
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/expanse/expanse/internal/errors"
)

// StorePath is a path in the Nix store.
type StorePath string

// SwitchMode selects what Switch activates.
type SwitchMode string

const (
	SwitchTest    SwitchMode = "test"   // activate, don't add boot entry
	SwitchBoot    SwitchMode = "boot"   // boot entry only, don't activate
	SwitchDefault SwitchMode = "switch" // both
)

// Generation is one system generation profile entry.
type Generation struct {
	Number int
	Path   StorePath
	Date   time.Time
}

// Driver drives nix on behalf of the agent.
type Driver interface {
	// Build realizes a flake attribute, returning the store path.
	// Build output is streamed line-by-line to logs (if non-nil).
	Build(ctx context.Context, flakeRef, attr string, logs io.Writer) (StorePath, error)
	// Switch activates a built system configuration.
	Switch(ctx context.Context, p StorePath, mode SwitchMode) error
	// CurrentSystem returns /run/current-system's target.
	CurrentSystem(ctx context.Context) (StorePath, error)
	// Generations lists system generations, oldest first.
	Generations(ctx context.Context) ([]Generation, error)
	// Rollback activates generation n (or the previous one if n == 0).
	Rollback(ctx context.Context, n int) error
	// GC collects garbage older than d.
	GC(ctx context.Context, keepDuration time.Duration) error
}

// ExecDriver shells out to the nix CLI. It is the production Driver.
type ExecDriver struct {
	// BuildTimeout bounds a single build (default 45 min, configurable).
	BuildTimeout time.Duration
	// PendingSwitchPath is the watchdog marker file. Before a switch the
	// previous system path is recorded there; after a successful switch
	// the file is cleared. If the agent dies mid-switch, the
	// expanse-switch-watchdog systemd unit sees the stale file and rolls
	// back + reboots.
	PendingSwitchPath string
	// Overridable paths / command constructor for tests.
	currentSystemPath string
	profilesPath      string
	runCmd            func(ctx context.Context, name string, args ...string) *exec.Cmd
}

// New creates the default driver.
func New() *ExecDriver {
	return &ExecDriver{
		BuildTimeout:      45 * time.Minute,
		PendingSwitchPath: "/persist/expanse/pending-switch",
		currentSystemPath: "/run/current-system",
		profilesPath:      "/nix/var/nix/profiles",
		runCmd: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return exec.CommandContext(ctx, name, args...)
		},
	}
}

func (d *ExecDriver) cmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	if d.runCmd != nil {
		return d.runCmd(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...)
}

func (d *ExecDriver) nixCmd(ctx context.Context, args ...string) *exec.Cmd {
	args = append([]string{
		"--extra-experimental-features", "nix-command flakes",
	}, args...)
	nixBin := "nix"
	if _, err := exec.LookPath(nixBin); err != nil {
		nixBin = "/run/current-system/sw/bin/nix"
	}
	c := d.cmd(ctx, nixBin, args...)
	// The agent may run with a read-only HOME (impermanence); nix needs
	// a writable cache dir. /tmp is always available.
	c.Env = append(os.Environ(), "XDG_CACHE_HOME=/var/cache/expanse-nix")
	return c
}

// Build implements Driver. Output is streamed line-by-line to logs so
// users see progress on long builds.
func (d *ExecDriver) Build(ctx context.Context, flakeRef, attr string, logs io.Writer) (StorePath, error) {
	if d.BuildTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d.BuildTimeout)
		defer cancel()
	}
	ref := flakeRef
	if attr != "" {
		ref = flakeRef + "#" + attr
	}
	cmd := d.nixCmd(ctx, "build", ref, "--print-build-logs", "--print-out-paths", "--no-link")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", fmt.Errorf("build pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("build pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("build start: %w", err)
	}
	// Stream build output line-by-line while the build runs; keep the tail
	// for error classification.
	var (
		errMu   sync.Mutex
		errTail = &bytes.Buffer{}
		wg      sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			if logs != nil {
				fmt.Fprintln(logs, sc.Text())
			}
			errMu.Lock()
			errTail.WriteString(sc.Text())
			errTail.WriteByte('\n')
			if errTail.Len() > 4096 {
				errTail.Next(errTail.Len() - 4096)
			}
			errMu.Unlock()
		}
	}()
	// --print-out-paths prints the resulting store path to stdout.
	var stdoutBuf strings.Builder
	_, _ = io.Copy(&stdoutBuf, stdout) // buffer writes cannot fail
	waitErr := cmd.Wait()
	wg.Wait()
	if waitErr != nil {
		errMu.Lock()
		msg := errTail.String()
		errMu.Unlock()
		if ctx.Err() != nil { // killed by our build timeout
			return "", errors.New(errors.KindTimeout, "nix.Build", "build timed out")
		}
		return "", classifyBuildError(waitErr, []byte(msg))
	}
	path := strings.TrimSpace(stdoutBuf.String())
	if path == "" {
		return "", errors.New(errors.KindInternal, "nix.Build", "no output path from nix build")
	}
	return StorePath(path), nil
}

// classifyBuildError maps nix failures to distinct error kinds.
func classifyBuildError(err error, out []byte) error {
	msg := string(out)
	if len(msg) > 512 {
		msg = msg[:512]
	}
	switch {
	case stderrors.Is(err, context.DeadlineExceeded) || stderrors.Is(err, context.Canceled):
		return errors.Wrap(err, errors.KindTimeout, "nix.Build", "build timed out")
	case strings.Contains(msg, "No space left on device"):
		return errors.New(errors.KindUnavailable, "nix.Build", "out of disk space: "+msg)
	case strings.Contains(msg, "error: unable to download") ||
		strings.Contains(msg, "fetch") && strings.Contains(msg, "failed"):
		return errors.New(errors.KindUnavailable, "nix.Build", "download failed: "+msg)
	case strings.Contains(msg, "error: syntax"):
		return errors.New(errors.KindInvalid, "nix.Build", "evaluation error: "+msg)
	default:
		return errors.Wrap(err, errors.KindInternal, "nix.Build", "build failed: "+msg)
	}
}

// Switch implements Driver. It records the previous system path in the
// pending-switch marker before activating, and clears it after success —
// the watchdog unit rolls back if the marker goes stale (D2.6).
func (d *ExecDriver) Switch(ctx context.Context, p StorePath, mode SwitchMode) error {
	sw := filepath.Join(string(p), "bin", "switch-to-configuration")
	if _, err := os.Stat(sw); err != nil {
		return errors.Wrap(err, errors.KindInvalid, "nix.Switch",
			fmt.Sprintf("store path %s has no switch-to-configuration (not a system closure?)", p))
	}
	if err := d.armWatchdog(ctx); err != nil {
		return fmt.Errorf("arm watchdog: %w", err)
	}
	cmd := d.cmd(ctx, sw, string(mode))
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Partial failures are common (a unit fails to restart): report a
		// degraded typed error, not a crash; leave the marker armed so the
		// watchdog can still roll back if the node is unreachable.
		return errors.Wrap(err, errors.KindUnavailable, "nix.Switch",
			fmt.Sprintf("switch-to-configuration %s failed (degraded): %s", mode, tail(out, 1024)))
	}
	if err := d.clearWatchdog(); err != nil {
		return fmt.Errorf("clear watchdog: %w", err)
	}
	return nil
}

// armWatchdog writes the pending-switch marker with the current system path.
func (d *ExecDriver) armWatchdog(ctx context.Context) error {
	if d.PendingSwitchPath == "" {
		return nil
	}
	cur, err := d.CurrentSystem(ctx)
	if err != nil {
		// Still arm the watchdog with an empty previous path; rollback will
		// use the previous generation instead.
		cur = ""
	}
	if err := os.MkdirAll(filepath.Dir(d.PendingSwitchPath), 0o750); err != nil {
		return err
	}
	return os.WriteFile(d.PendingSwitchPath, []byte(fmt.Sprintf("%s\n%d\n", cur, time.Now().Unix())), 0o644)
}

// clearWatchdog removes the marker after a successful switch.
func (d *ExecDriver) clearWatchdog() error {
	if d.PendingSwitchPath == "" {
		return nil
	}
	return os.Remove(d.PendingSwitchPath)
}

// WatchdogPending reports the previous system path recorded in a stale
// pending-switch marker, if any (used by the watchdog unit and health).
func (d *ExecDriver) WatchdogPending() (StorePath, bool) {
	data, err := os.ReadFile(d.PendingSwitchPath)
	if err != nil {
		return "", false
	}
	lines := strings.SplitN(strings.TrimSpace(string(data)), "\n", 2)
	return StorePath(lines[0]), true
}

// CurrentSystem implements Driver.
func (d *ExecDriver) CurrentSystem(ctx context.Context) (StorePath, error) {
	// /run/current-system is a symlink into the store.
	p, err := os.Readlink(d.currentSystem())
	if err != nil {
		return "", errors.Wrap(err, errors.KindInternal, "nix.CurrentSystem", "read /run/current-system")
	}
	if p == "" {
		return "", errors.New(errors.KindInternal, "nix.CurrentSystem", "empty current-system link")
	}
	return StorePath(p), nil
}

func (d *ExecDriver) currentSystem() string {
	if d.currentSystemPath != "" {
		return d.currentSystemPath
	}
	return "/run/current-system"
}

// Generations implements Driver. Reads the profile links directly —
// structured data, no human output parsing.
func (d *ExecDriver) Generations(ctx context.Context) ([]Generation, error) {
	profiles := d.profilesPath
	if profiles == "" {
		profiles = "/nix/var/nix/profiles"
	}
	matches, err := filepath.Glob(filepath.Join(profiles, "system-*-link"))
	if err != nil {
		return nil, err
	}
	var gens []Generation
	for _, m := range matches {
		base := filepath.Base(m)
		base = strings.TrimSuffix(base, "-link")
		base = strings.TrimPrefix(base, "system-")
		n, err := strconv.Atoi(base)
		if err != nil {
			continue
		}
		target, err := os.Readlink(m)
		if err != nil {
			continue
		}
		st, err := os.Lstat(m)
		if err != nil {
			continue
		}
		gens = append(gens, Generation{Number: n, Path: StorePath(target), Date: st.ModTime()})
	}
	sort.Slice(gens, func(i, j int) bool { return gens[i].Number < gens[j].Number })
	return gens, nil
}

// Rollback implements Driver. n==0 means "previous generation".
func (d *ExecDriver) Rollback(ctx context.Context, n int) error {
	gens, err := d.Generations(ctx)
	if err != nil || len(gens) == 0 {
		return errors.New(errors.KindNotFound, "nix.Rollback", "no generations to roll back to")
	}
	var target Generation
	if n == 0 {
		// Previous relative to the current system.
		cur, err := d.CurrentSystem(ctx)
		if err != nil {
			return err
		}
		idx := -1
		for i, g := range gens {
			if g.Path == cur {
				idx = i
				break
			}
		}
		if idx <= 0 {
			return errors.New(errors.KindNotFound, "nix.Rollback", "no previous generation to roll back to")
		}
		target = gens[idx-1]
	} else {
		found := false
		for _, g := range gens {
			if g.Number == n {
				target = g
				found = true
				break
			}
		}
		if !found {
			return errors.New(errors.KindNotFound, "nix.Rollback",
				fmt.Sprintf("generation %d not found", n))
		}
	}
	return d.Switch(ctx, target.Path, SwitchDefault)
}

// GC implements Driver.
func (d *ExecDriver) GC(ctx context.Context, keepDuration time.Duration) error {
	cmd := d.nixCmd(ctx, "store", "gc")
	if keepDuration > 0 {
		cmd = d.cmd(ctx, "nix-collect-garbage", "--delete-older-than", keepDuration.String())
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return errors.Wrap(err, errors.KindUnavailable, "nix.GC", "garbage collection failed: "+tail(out, 512))
	}
	return nil
}

func tail(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		s = s[len(s)-n:]
	}
	return s
}
