// Package testsock gives tests unix socket paths short enough to bind.
package testsock

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Path returns name in a fresh directory under /tmp, removed when t ends. t.TempDir()
// follows TMPDIR, which can push a socket past the 108-byte sun_path limit (CI's nix shell).
func Path(t testing.TB, name string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "sock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, name)
}

// Accepting waits until a connection to the unix socket at path succeeds. The socket file
// appears at bind, before listen, so its existence alone does not mean a server is ready.
func Accepting(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		c, err := net.Dial("unix", path)
		if err == nil {
			return c.Close()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s not accepting after %v: %w", path, timeout, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
