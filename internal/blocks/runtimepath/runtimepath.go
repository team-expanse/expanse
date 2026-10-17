// Package runtimepath locates a block type's realised runtime: the agent links it here (also its GC
// root), and expanse-block-run puts its bin/ first on PATH.
package runtimepath

import (
	"os"
	"path/filepath"
	"strings"
)

// Dir holds one link per block type; links inside the gcroots tree are direct GC roots.
const Dir = "/nix/var/nix/gcroots/expanse/block-runtime"

// Link is the runtime link for blockType under dir, named like its flake attr ("db/mariadb" -> "db-mariadb").
func Link(dir, blockType string) string {
	return filepath.Join(dir, strings.ReplaceAll(blockType, "/", "-"))
}

// Prepend returns path with blockType's runtime bin/ in front, or path unchanged when it has none.
func Prepend(dir, blockType, path string) string {
	bin := filepath.Join(Link(dir, blockType), "bin")
	if st, err := os.Stat(bin); err != nil || !st.IsDir() {
		return path
	}
	if path == "" {
		return bin
	}
	return bin + ":" + path
}
