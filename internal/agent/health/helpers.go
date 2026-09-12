package health

import (
	"context"
	"os"
	"os/exec"
	"runtime"
)

func readFile(path string) ([]byte, error) { return os.ReadFile(path) }

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

func runtimeNumCPU() int { return runtime.NumCPU() }
