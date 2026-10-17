package agent

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/expanse/expanse/internal/agent/nix"
	"github.com/expanse/expanse/internal/blocks/runtimepath"
)

type fakeNixBuild struct{ calls []string }

func (f *fakeNixBuild) BuildFile(_ context.Context, file, attr string, _ io.Writer) (nix.StorePath, error) {
	f.calls = append(f.calls, file+" "+attr)
	return "/nix/store/aaa-expanse-runtime-db-mariadb", nil
}

type fakeFallback struct{ types []string }

func (f *fakeFallback) Build(_ context.Context, blockType string) (nix.StorePath, error) {
	f.types = append(f.types, blockType)
	return "/nix/store/stub", nil
}

func newTestRuntimeBuilder(t *testing.T, outs map[string]string) (*runtimeBuilder, *fakeNixBuild, *fakeFallback) {
	t.Helper()
	nb, fb := &fakeNixBuild{}, &fakeFallback{}
	return &runtimeBuilder{outs: outs, expr: "/etc/expanse/block-runtimes.nix", nix: nb, dir: t.TempDir(), fallback: fb}, nb, fb
}

// A runtime already in the store (built into the node, or substituted earlier) is linked without evaluating.
func TestRuntimeBuilderLinksPresentRuntime(t *testing.T) {
	present := t.TempDir()
	b, nb, _ := newTestRuntimeBuilder(t, map[string]string{"vm/instance": present})
	p, err := b.Build(context.Background(), "vm/instance")
	if err != nil || string(p) != present || len(nb.calls) != 0 {
		t.Fatalf("Build = %q, %v; nix calls %v", p, err, nb.calls)
	}
	if got, _ := os.Readlink(runtimepath.Link(b.dir, "vm/instance")); got != present {
		t.Fatalf("GC root link = %q, want %q", got, present)
	}
}

// A missing runtime is built from the node's expression, quoted since type names hold a slash.
func TestRuntimeBuilderBuildsMissingRuntime(t *testing.T) {
	b, nb, _ := newTestRuntimeBuilder(t, map[string]string{"db/mariadb": "/nix/store/not-here"})
	p, err := b.Build(context.Background(), "db/mariadb")
	if err != nil || p != "/nix/store/aaa-expanse-runtime-db-mariadb" {
		t.Fatalf("Build = %q, %v", p, err)
	}
	if len(nb.calls) != 1 || nb.calls[0] != `/etc/expanse/block-runtimes.nix "db/mariadb"` {
		t.Fatalf("nix calls %v", nb.calls)
	}
	if got, _ := os.Readlink(runtimepath.Link(b.dir, "db/mariadb")); got != string(p) {
		t.Fatalf("GC root link = %q", got)
	}
	if b.Key("db/mariadb") != "/nix/store/not-here" {
		t.Fatalf("Key = %q", b.Key("db/mariadb"))
	}
}

func TestRuntimeBuilderRelinksOnUpgrade(t *testing.T) {
	present := t.TempDir()
	b, _, _ := newTestRuntimeBuilder(t, map[string]string{"vm/instance": present})
	link := runtimepath.Link(b.dir, "vm/instance")
	if err := os.Symlink("/nix/store/old", link); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Build(context.Background(), "vm/instance"); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got, _ := os.Readlink(link); got != present {
		t.Fatalf("link still %q", got)
	}
}

func TestRuntimeBuilderFallsBackForTypesWithoutRuntime(t *testing.T) {
	b, nb, fb := newTestRuntimeBuilder(t, map[string]string{})
	if _, err := b.Build(context.Background(), "util/echo"); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(fb.types) != 1 || len(nb.calls) != 0 || b.Key("util/echo") != "" {
		t.Fatalf("fallback %v, nix %v", fb.types, nb.calls)
	}
}

func TestLoadRuntimes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "block-runtimes.json")
	if m, err := loadRuntimes(path); err != nil || len(m) != 0 {
		t.Fatalf("missing file = %v, %v; want empty", m, err)
	}
	if err := os.WriteFile(path, []byte(`{"vm/instance":"/nix/store/q-runtime"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if m, err := loadRuntimes(path); err != nil || m["vm/instance"] != "/nix/store/q-runtime" {
		t.Fatalf("loadRuntimes = %v, %v", m, err)
	}
}
