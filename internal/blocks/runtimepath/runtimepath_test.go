package runtimepath

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLinkUsesFlakeStyleAttr(t *testing.T) {
	if got, want := Link("/roots", "db/mariadb"), "/roots/db-mariadb"; got != want {
		t.Fatalf("Link = %q, want %q", got, want)
	}
}

func TestPrependAddsRuntimeBinFirst(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(Link(dir, "vm/instance"), "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, want := Prepend(dir, "vm/instance", "/run/current-system/sw/bin"), bin+":/run/current-system/sw/bin"; got != want {
		t.Fatalf("Prepend = %q, want %q", got, want)
	}
}

func TestPrependLeavesPathWithoutRuntime(t *testing.T) {
	if got := Prepend(t.TempDir(), "util/echo", "/usr/bin"); got != "/usr/bin" {
		t.Fatalf("Prepend = %q, want PATH unchanged", got)
	}
}
