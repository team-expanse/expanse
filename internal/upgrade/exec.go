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
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "nix-build", buildArgs(file, substituter, e.PublicKey)...)
	cmd.Stdout, cmd.Stderr = &out, logs
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("nix-build %s: %w", file, err)
	}
	return strings.TrimSpace(out.String()), nil
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
