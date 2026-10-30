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

func TestDeclaredBinFollowsTheNodesRuntimeList(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, "env")
	real := filepath.Join(dir, "qemu", "bin", "qemu-kvm")
	if err := os.MkdirAll(filepath.Join(env, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(env, "bin", "qemu-kvm")); err != nil {
		t.Fatal(err)
	}
	list := filepath.Join(dir, "block-runtimes.json")
	if err := os.WriteFile(list, []byte(`{"vm/instance":"`+env+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := DeclaredBin(list, "vm/instance", "qemu-kvm"); err != nil || got != real {
		t.Errorf("DeclaredBin = %q, %v; want %q", got, err, real)
	}
	if _, err := DeclaredBin(list, "web/nginx", "nginx"); err == nil {
		t.Error("a type the list lacks resolved")
	}
}
