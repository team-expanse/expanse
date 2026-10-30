// Package runtimepath locates a block type's realised runtime: the agent links it here (also its GC
// root), and expanse-block-run puts its bin/ first on PATH.
package runtimepath

import (
	"encoding/json"
	"fmt"
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

// Declared lists the runtime each block type should use, as the node's current system says.
const Declared = "/etc/expanse/block-runtimes.json"

// DeclaredBin resolves name in blockType's runtime from the list at file, through any symlinks.
func DeclaredBin(file, blockType, name string) (string, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	var outs map[string]string
	if err := json.Unmarshal(raw, &outs); err != nil {
		return "", err
	}
	out, ok := outs[blockType]
	if !ok {
		return "", fmt.Errorf("%s lists no runtime for %s", file, blockType)
	}
	return filepath.EvalSymlinks(filepath.Join(out, "bin", name))
}
