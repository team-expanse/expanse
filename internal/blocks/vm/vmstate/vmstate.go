// Package vmstate keeps a suspended VM's saved RAM on its node, one directory per replica:
// "state" is qemu's migration stream and "token" names the suspension it belongs to.
package vmstate

import (
	"os"
	"path/filepath"
	"strings"
)

// DefaultDir holds every replica's saved state, beside the agent's data so it outlives a reboot.
const DefaultDir = "/persist/expanse/vm-state"

// Dir is instance's state directory under base (DefaultDir when empty).
func Dir(base, instance string) string {
	if base == "" {
		base = DefaultDir
	}
	return filepath.Join(base, instance)
}

// StatePath is the committed migration stream.
func StatePath(dir string) string { return filepath.Join(dir, "state") }

// TempPath is where qemu writes a save before Commit.
func TempPath(dir string) string { return filepath.Join(dir, "state.tmp") }

func tokenPath(dir string) string { return filepath.Join(dir, "token") }

// Token names the committed save in dir, "" when there is none.
func Token(dir string) string {
	if _, err := os.Stat(StatePath(dir)); err != nil {
		return ""
	}
	raw, err := os.ReadFile(tokenPath(dir))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// Commit makes the save at TempPath the state for token. The old token goes first, so a
// crash part way never pairs a token with another suspension's state.
func Commit(dir, token string) error {
	if err := os.Remove(tokenPath(dir)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := syncFile(TempPath(dir)); err != nil {
		return err
	}
	if err := os.Rename(TempPath(dir), StatePath(dir)); err != nil {
		return err
	}
	tmp := tokenPath(dir) + ".tmp"
	if err := os.WriteFile(tmp, []byte(token+"\n"), 0o600); err != nil {
		return err
	}
	if err := syncFile(tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, tokenPath(dir)); err != nil {
		return err
	}
	return syncFile(dir)
}

// Remove deletes instance's saved state, if any.
func Remove(dir string) error { return os.RemoveAll(dir) }

func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
