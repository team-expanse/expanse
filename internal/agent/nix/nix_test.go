package nix

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/errors"
)

// fakeCmd runs a tiny shell script instead of real nix.
func fakeDriver(t *testing.T, script string) *ExecDriver {
	t.Helper()
	d := New()
	d.runCmd = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", script)
	}
	return d
}

func TestBuildSuccessParsesOutPath(t *testing.T) {
	d := fakeDriver(t, `echo "building..." >&2; echo "/nix/store/abc-system"`)
	p, err := d.Build(context.Background(), "path:/etc/nixos", "toplevel", nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if p != "/nix/store/abc-system" {
		t.Errorf("path = %q", p)
	}
}

func TestBuildFailureErrorKinds(t *testing.T) {
	cases := []struct {
		script string
		want   errors.Kind
	}{
		{`echo "error: ... No space left on device" >&2; exit 1`, errors.KindUnavailable},
		{`echo "error: unable to download 'https://x': timeout" >&2; exit 1`, errors.KindUnavailable},
		{`echo "error: syntax error, unexpected ID" >&2; exit 1`, errors.KindInvalid},
	}
	for i, c := range cases {
		d := fakeDriver(t, c.script)
		_, err := d.Build(context.Background(), "path:/etc/nixos", "toplevel", nil)
		if err == nil {
			t.Fatalf("case %d: expected error", i)
		}
		if got := errors.KindOf(err); got != c.want {
			t.Errorf("case %d: kind = %q, want %q (%v)", i, got, c.want, err)
		}
	}
}

func TestBuildTimeoutKind(t *testing.T) {
	d := New()
	d.BuildTimeout = 50 * time.Millisecond
	d.runCmd = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sleep", "5")
	}
	_, err := d.Build(context.Background(), "path:/etc/nixos", "toplevel", nil)
	if !errors.Is(err, errors.KindTimeout) {
		t.Errorf("kind = %q, want timeout (%v)", errors.KindOf(err), err)
	}
}

func TestBuildStreamsLogs(t *testing.T) {
	d := fakeDriver(t, `for i in 1 2 3; do echo "log line $i" >&2; done; echo /nix/store/x`)
	var logs strings.Builder
	if _, err := d.Build(context.Background(), "f", "a", &logs); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "log line 3") {
		t.Errorf("streamed logs = %q, want all lines", logs.String())
	}
}

// switchFixture sets up a fake system closure, current-system link,
// generation profiles, and pending-switch path.
func switchFixture(t *testing.T) (*ExecDriver, string) {
	t.Helper()
	root := t.TempDir()
	csDir := filepath.Join(root, "run")
	os.MkdirAll(csDir, 0o755)

	// Fake generation profiles pointing at real closure dirs, whose
	// switch-to-configuration scripts flip current-system to themselves.
	profiles := filepath.Join(root, "profiles")
	os.MkdirAll(profiles, 0o755)
	makeClosure := func(name string) string {
		closure := filepath.Join(root, name)
		os.MkdirAll(filepath.Join(closure, "bin"), 0o755)
		os.WriteFile(filepath.Join(closure, "bin", "switch-to-configuration"),
			[]byte(fmt.Sprintf("#!/bin/sh\nln -sfn %s %s\n", closure, filepath.Join(csDir, "current-system"))), 0o755)
		return closure
	}
	oldClosure := makeClosure("old-system")
	newClosure := makeClosure("new-system")
	os.Symlink(oldClosure, filepath.Join(profiles, "system-1-link"))
	os.Symlink(newClosure, filepath.Join(profiles, "system-2-link"))
	// /run/current-system initially points at the old closure.
	_ = os.Remove(filepath.Join(csDir, "current-system"))
	os.Symlink(oldClosure, filepath.Join(csDir, "current-system"))

	d := New()
	d.currentSystemPath = filepath.Join(csDir, "current-system")
	d.profilesPath = profiles
	d.PendingSwitchPath = filepath.Join(root, "pending-switch")
	// "switch" runs the real switch-to-configuration script; other
	// commands flip current-system to new-system (a fake build/switch).
	d.runCmd = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if strings.HasSuffix(name, "switch-to-configuration") {
			return exec.CommandContext(ctx, "sh", append([]string{name}, args...)...)
		}
		script := fmt.Sprintf("ln -sfn /nix/store/new-system %s", d.currentSystemPath)
		_ = args
		return exec.CommandContext(ctx, "sh", "-c", script)
	}
	return d, root
}

func TestSwitchArmsAndClearsWatchdog(t *testing.T) {
	d, root := switchFixture(t)

	// A fake system closure with bin/switch-to-configuration.
	closure := filepath.Join(root, "closure")
	os.MkdirAll(filepath.Join(closure, "bin"), 0o755)
	sw := filepath.Join(closure, "bin", "switch-to-configuration")
	os.WriteFile(sw, []byte("#!/bin/sh\nexit 0\n"), 0o755)

	if err := d.Switch(context.Background(), StorePath(closure), SwitchDefault); err != nil {
		t.Fatalf("Switch: %v", err)
	}
	if _, err := os.Stat(d.PendingSwitchPath); !os.IsNotExist(err) {
		t.Errorf("pending-switch marker not cleared after success (err=%v)", err)
	}
}

func TestSwitchFailureKeepsWatchdogArmed(t *testing.T) {
	d, root := switchFixture(t)
	closure := filepath.Join(root, "closure")
	os.MkdirAll(filepath.Join(closure, "bin"), 0o755)
	os.WriteFile(filepath.Join(closure, "bin", "switch-to-configuration"),
		[]byte("#!/bin/sh\necho 'unit foo.service failed' >&2\nexit 1\n"), 0o755)

	err := d.Switch(context.Background(), StorePath(closure), SwitchDefault)
	if !errors.Is(err, errors.KindUnavailable) {
		t.Errorf("partial switch failure kind = %q, want unavailable (degraded): %v", errors.KindOf(err), err)
	}
	if _, statErr := os.Stat(d.PendingSwitchPath); statErr != nil {
		t.Errorf("watchdog marker must stay armed on degraded switch: %v", statErr)
	}
}

func TestGenerationsAndRollback(t *testing.T) {
	d, root := switchFixture(t)
	gens, err := d.Generations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(gens) != 2 || gens[0].Number != 1 || gens[1].Number != 2 {
		t.Fatalf("generations = %+v", gens)
	}
	if gens[0].Path != StorePath(filepath.Join(root, "old-system")) ||
		gens[1].Path != StorePath(filepath.Join(root, "new-system")) {
		t.Errorf("gen paths = %q, %q", gens[0].Path, gens[1].Path)
	}

	// Rollback to generation 2: the closure's switch script flips
	// current-system to itself, so current must become new-system.
	if err := d.Rollback(context.Background(), 2); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	cur, _ := d.CurrentSystem(context.Background())
	if cur != StorePath(filepath.Join(root, "new-system")) {
		t.Errorf("current after rollback = %q, want new-system closure", cur)
	}
	// Rollback to a missing generation must be KindNotFound.
	if err := d.Rollback(context.Background(), 99); !errors.Is(err, errors.KindNotFound) {
		t.Errorf("rollback to missing generation kind = %q, want not_found", errors.KindOf(err))
	}
}
