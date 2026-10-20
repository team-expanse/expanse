package upgrade

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

// writeTarball makes a git-archive-shaped tarball: every entry under one top directory.
func writeTarball(t *testing.T, entries map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "src.tar.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, body := range entries {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if body == dirEntry {
			hdr = &tar.Header{Name: name, Mode: 0o755, Typeflag: tar.TypeDir}
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if body != dirEntry {
			_, _ = tw.Write([]byte(body))
		}
	}
	tw.Close()
	gz.Close()
	f.Close()
	return path
}

func release(version string) map[string]string {
	return map[string]string{
		"expanse-" + version + "/":                dirEntry,
		"expanse-" + version + "/nix/version.nix": `"` + version + `"` + "\n",
		"expanse-" + version + "/flake.lock":      "{}",
	}
}

const dirEntry = "<dir>"

func TestInstallSwapsTheSourceAndKeepsThePrevious(t *testing.T) {
	nixos := t.TempDir()
	cur := filepath.Join(nixos, "expanse")
	os.MkdirAll(filepath.Join(cur, "nix"), 0o755)
	os.WriteFile(filepath.Join(cur, "nix/version.nix"), []byte(`"1.11.3"`), 0o644)

	tree := &Tree{Dir: nixos}
	if err := tree.Install(writeTarball(t, release("1.12.0")), "1.12.0"); err != nil {
		t.Fatal(err)
	}
	if v, _ := tree.Version("expanse"); v != "1.12.0" {
		t.Errorf("installed source is %q", v)
	}
	if v, _ := tree.Version("expanse.prev"); v != "1.11.3" {
		t.Errorf("previous source is %q", v)
	}
	if err := tree.Restore(); err != nil {
		t.Fatal(err)
	}
	if v, _ := tree.Version("expanse"); v != "1.11.3" {
		t.Errorf("after restore the source is %q", v)
	}
}

func TestInstallRefusesATarballOfAnotherVersion(t *testing.T) {
	tree := &Tree{Dir: t.TempDir()}
	if err := tree.Install(writeTarball(t, release("1.11.0")), "1.12.0"); err == nil {
		t.Fatal("a 1.11.0 tarball was installed as 1.12.0")
	}
	if _, err := os.Stat(filepath.Join(tree.Dir, "expanse")); !os.IsNotExist(err) {
		t.Error("a refused tarball still replaced the source")
	}
}

func TestInstallRefusesEntriesOutsideTheTree(t *testing.T) {
	for _, name := range []string{"expanse-1.12.0/../../etc/passwd", "/etc/passwd", "other/x"} {
		entries := release("1.12.0")
		entries[name] = "x"
		tree := &Tree{Dir: t.TempDir()}
		if err := tree.Install(writeTarball(t, entries), "1.12.0"); err == nil {
			t.Errorf("entry %q accepted", name)
		}
	}
}

func TestInstallOnANodeWithoutASourceYet(t *testing.T) {
	tree := &Tree{Dir: t.TempDir()} // installed with 1.10.0: imports /etc/expanse/flake
	if err := tree.Install(writeTarball(t, release("1.12.0")), "1.12.0"); err != nil {
		t.Fatal(err)
	}
	if v, _ := tree.Version("expanse"); v != "1.12.0" {
		t.Errorf("installed source is %q", v)
	}
}

func TestRestoreAfterAFirstInstallLeavesNoSource(t *testing.T) {
	tree := &Tree{Dir: t.TempDir()}
	if err := tree.Install(writeTarball(t, release("1.12.0")), "1.12.0"); err != nil {
		t.Fatal(err)
	}
	if err := tree.Restore(); err != nil {
		t.Fatalf("restoring a node that had no source: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tree.Dir, "expanse")); !os.IsNotExist(err) {
		t.Error("the failed release's source was left in place")
	}
}
