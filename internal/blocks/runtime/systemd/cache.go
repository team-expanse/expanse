// Closure cache (§5.3 "Optimization that matters"): hash the nix closure
// inputs; when unchanged, skip build AND switch entirely — a replicas-only
// spec change must apply in < 5 s. The hash deliberately EXCLUDES the
// replica index and the intended unit count: the nix closure carries the
// template unit; replicas are instances. It also excludes credentials'
// source paths? No — credential SOURCES are node-local paths that do not
// change the built closure, but their NAMES do (LoadCredential lines are
// in the template). VolumeMounts likewise appear in the template, so they
// are inputs. Index is the only field that is not.
package systemd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	nix "github.com/expanse/expanse/internal/agent/nix"
	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/quantity"
)

// hashable is the closure-input projection of a Spec: everything that
// changes the built closure. Replica index intentionally absent.
type hashable struct {
	Type                 string            `json:"type"`
	Args                 []string          `json:"args,omitempty"`
	CPUQuotaMilli        int64             `json:"cpuQuotaMilli,omitempty"`
	MemoryMax            int64             `json:"memoryMax,omitempty"`
	VolumeMounts         []string          `json:"volumeMounts,omitempty"`
	Credentials          map[string]string `json:"credentials,omitempty"`
	ExtraAddressFamilies []string          `json:"extraAddressFamilies,omitempty"`
	IOWeight             int               `json:"ioWeight,omitempty"`
	TasksMax             int               `json:"tasksMax,omitempty"`
	StaticUID            int               `json:"staticUid,omitempty"`
}

// project converts a Spec to its hashable form.
func project(s Spec) hashable {
	h := hashable{
		Type:                 s.Type,
		Args:                 s.Args,
		VolumeMounts:         s.VolumeMounts,
		Credentials:          s.Credentials,
		ExtraAddressFamilies: s.ExtraAddressFamilies,
		IOWeight:             s.IOWeight,
		TasksMax:             s.TasksMax,
		StaticUID:            s.StaticUID,
	}
	if s.Limits != nil {
		if s.Limits.Cpu != "" {
			if q, err := parseCPU(s.Limits.Cpu); err == nil {
				h.CPUQuotaMilli = q.Milli
			}
		}
		if s.Limits.Memory != "" {
			if q, err := parseBytes(s.Limits.Memory); err == nil {
				h.MemoryMax = q.N
			}
		}
	}
	return h
}

// ClosureHash computes the canonical closure-inputs hash for a spec.
func ClosureHash(s Spec) string {
	b, err := json.Marshal(project(s))
	if err != nil {
		// hashable is plain JSON types; this cannot happen.
		panic(fmt.Sprintf("closure hash: %v", err))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func parseCPU(v string) (quantity.CPU, error) { return quantity.ParseCPU(v) }

func parseBytes(v string) (quantity.Bytes, error) { return quantity.ParseBytes(v) }

// Cache persists closure-hash -> store-path entries so the agent skips
// rebuilds across restarts. File-backed JSON under the agent state dir.
type Cache struct {
	mu   sync.Mutex
	path string
	ents map[string]string
}

// NewCache opens (or creates) a cache persisted at path.
func NewCache(path string) (*Cache, error) {
	c := &Cache{path: path, ents: map[string]string{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return nil, errors.Wrap(err, errors.KindInternal, "systemd.NewCache", "read cache")
	}
	if err := json.Unmarshal(b, &c.ents); err != nil {
		// Torn cache (power loss mid-write) is recoverable: entries are
		// pure memoization. Log-and-reset, not a hard failure — the
		// node just rebuilds the closures.
		return c, nil
	}
	return c, nil
}

// Get returns the store path for a closure hash, if cached.
func (c *Cache) Get(hash string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.ents[hash]
	return p, ok
}

// Put records a built closure for hash and persists the cache.
func (c *Cache) Put(hash, storePath string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ents[hash] = storePath
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return errors.Wrap(err, errors.KindInternal, "systemd.Cache.Put", "mkdir")
	}
	b, err := json.Marshal(c.ents)
	if err != nil {
		return errors.Wrap(err, errors.KindInternal, "systemd.Cache.Put", "marshal")
	}
	if err := writeFileSync(c.path, b, 0o600); err != nil {
		return errors.Wrap(err, errors.KindInternal, "systemd.Cache.Put", "write")
	}
	return nil
}

// Builder is the build surface of the nix driver (Phase 02) the applier
// needs; tests supply mocks and count invocations.
type Builder interface {
	// Build realizes the block's flake attribute, returning the store path.
	Build(ctx context.Context, blockType string) (nix.StorePath, error)
}

// Applier applies a replica's desired state: build the closure when the
// hash misses (and remember it), short-circuit when it hits. It returns
// whether a build happened so callers can decide about switching.
type Applier struct {
	Cache   *Cache
	Builder Builder
}

// ApplyResult reports what Apply did.
type ApplyResult struct {
	StorePath string
	// Built is true when the nix build ran (cache miss). §5.3: a
	// replicas-only change must yield Built=false.
	Built bool
}

// Apply resolves the closure for a replica spec.
func (a *Applier) Apply(ctx context.Context, s Spec) (ApplyResult, error) {
	hash := ClosureHash(s)
	if p, ok := a.Cache.Get(hash); ok {
		return ApplyResult{StorePath: p, Built: false}, nil
	}
	p, err := a.Builder.Build(ctx, s.Type)
	if err != nil {
		return ApplyResult{}, err
	}
	if err := a.Cache.Put(hash, string(p)); err != nil {
		return ApplyResult{}, err
	}
	return ApplyResult{StorePath: string(p), Built: true}, nil
}

// writeFileSync: temp file + fsync + rename — the cache file must not
// tear under power loss (a torn write used to brick the block runtime:
// "corrupt cache" disabled replicas until manual cleanup).
func writeFileSync(path string, b []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".cache-tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
