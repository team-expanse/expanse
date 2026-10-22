package upgrade

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/expanse/expanse/internal/agent/nix"
)

// ExecNix is the production Nix: nix-build, nix-env and the agent's watchdog-armed switch.
type ExecNix struct {
	// PublicKey is trusted for the release cache only.
	PublicKey string
	Driver    *nix.ExecDriver
}

// BuildSystem implements Nix.
func (e *ExecNix) BuildSystem(ctx context.Context, file, substituter string, logs io.Writer) (string, error) {
	runs := 0
	path, err := retryKilled(ctx, func() (string, string, error) {
		if runs++; runs > 1 {
			fmt.Fprintln(logs, "expanse: a signal killed the build; building once more")
		}
		var out, stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, "nix-build", buildArgs(file, substituter, e.PublicKey)...)
		cmd.Stdout, cmd.Stderr = &out, io.MultiWriter(logs, &stderr)
		err := cmd.Run()
		return strings.TrimSpace(out.String()), stderr.String(), err
	})
	if err != nil {
		return "", fmt.Errorf("nix-build %s: %w", file, err)
	}
	return path, nil
}

// retryKilled runs a build once more when a signal killed its builder; seen rarely in VM tests, cause unknown.
func retryKilled(ctx context.Context, build func() (path, stderr string, err error)) (string, error) {
	path, stderr, err := build()
	if err != nil && ctx.Err() == nil && strings.Contains(stderr, "failed due to signal") {
		path, _, err = build()
	}
	return path, err
}

func buildArgs(file, substituter, key string) []string {
	args := []string{file, "-A", "config.system.build.toplevel", "--no-out-link"}
	if substituter != "" {
		args = append(args, "--option", "extra-substituters", substituter,
			"--option", "extra-trusted-public-keys", key)
	}
	return args
}

// SetProfile implements Nix: the new system becomes the next boot's default generation.
func (e *ExecNix) SetProfile(ctx context.Context, path string) error {
	out, err := exec.CommandContext(ctx, "nix-env", "-p", "/nix/var/nix/profiles/system", "--set", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("nix-env --set: %w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

// Switch implements Nix.
func (e *ExecNix) Switch(ctx context.Context, path string) error {
	return e.Driver.Switch(ctx, nix.StorePath(path), nix.SwitchDefault)
}
