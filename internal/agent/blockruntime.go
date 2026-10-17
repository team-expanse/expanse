package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"

	"github.com/expanse/expanse/internal/agent/nix"
	"github.com/expanse/expanse/internal/blocks/runtime/systemd"
	"github.com/expanse/expanse/internal/blocks/runtimepath"
)

// nixBuilder is the slice of nix.ExecDriver the runtime builder needs.
type nixBuilder interface {
	BuildFile(ctx context.Context, file, attr string, logs io.Writer) (nix.StorePath, error)
}

// runtimeBuilder links a block type's runtime as a GC root where expanse-block-run looks, first
// building it from the node's expression when it is not in the store yet; types without one go to
// the fallback builder.
type runtimeBuilder struct {
	outs     map[string]string // block type -> runtime out path (/etc/expanse/block-runtimes.json)
	expr     string            // /etc/expanse/block-runtimes.nix: the same runtimes on the node's nixpkgs
	nix      nixBuilder
	dir      string
	fallback systemd.Builder
	logger   *slog.Logger
}

// Key is the type's runtime out path, so an upgrade that changes it misses the closure cache.
func (b *runtimeBuilder) Key(blockType string) string { return b.outs[blockType] }

func (b *runtimeBuilder) Build(ctx context.Context, blockType string) (nix.StorePath, error) {
	out, ok := b.outs[blockType]
	if !ok {
		return b.fallback.Build(ctx, blockType)
	}
	p := nix.StorePath(out)
	if _, err := os.Stat(out); err != nil {
		built, err := b.nix.BuildFile(ctx, b.expr, strconv.Quote(blockType), os.Stdout)
		if err != nil {
			if b.logger != nil {
				b.logger.Error("block runtime build failed", "type", blockType, "err", err)
			}
			return "", fmt.Errorf("build %s runtime (the first one on a node needs a binary cache or network): %w", blockType, err)
		}
		p = built
	}
	if err := relink(runtimepath.Link(b.dir, blockType), string(p)); err != nil {
		return "", fmt.Errorf("root %s runtime: %w", blockType, err)
	}
	return p, nil
}

// relink points link at target atomically (symlink beside it, then rename over it).
func relink(link, target string) error {
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return err
	}
	tmp := link + ".new"
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, link)
}

// loadRuntimes reads the type -> out path map the node module writes; a missing file means none.
func loadRuntimes(path string) (map[string]string, error) {
	m := map[string]string{}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}
