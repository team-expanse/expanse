// Package testsock gives tests unix socket paths short enough to bind.
package testsock

import (
	"os"
	"path/filepath"
	"testing"
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
