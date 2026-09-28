package health

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func readFile(path string) ([]byte, error) { return os.ReadFile(path) }

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

func runtimeNumCPU() int { return runtime.NumCPU() }

// systemUptime is the time since boot; unreadable reads as long past, so no grace hides a problem.
func systemUptime() time.Duration {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 24 * time.Hour
	}
	secs, err := strconv.ParseFloat(strings.Fields(string(b) + " 0")[0], 64)
	if err != nil {
		return 24 * time.Hour
	}
	return time.Duration(secs * float64(time.Second))
}
