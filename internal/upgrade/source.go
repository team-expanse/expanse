package upgrade

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Tree is the node's NixOS config directory: configuration.nix imports ./expanse, the source
// copy of the running release; expanse.prev is the one before it.
type Tree struct {
	Dir string // /etc/nixos
}

const (
	currentDir  = "expanse"
	previousDir = "expanse.prev"
	nextDir     = "expanse.next"
)

// Install unpacks a release tarball as the node's source, keeping the current one as expanse.prev.
func (t *Tree) Install(tarball, version string) error {
	next := filepath.Join(t.Dir, nextDir)
	if err := os.RemoveAll(next); err != nil {
		return err
	}
	if err := unpack(tarball, "expanse-"+version, next); err != nil {
		_ = os.RemoveAll(next)
		return err
	}
	if got, err := t.Version(nextDir); err != nil || got != version {
		_ = os.RemoveAll(next)
		return fmt.Errorf("the tarball holds release %q, not %s: %w", got, version, err)
	}
	if err := os.RemoveAll(filepath.Join(t.Dir, previousDir)); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(t.Dir, currentDir), filepath.Join(t.Dir, previousDir)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Rename(next, filepath.Join(t.Dir, currentDir))
}

// Restore puts expanse.prev back as the node's source.
func (t *Tree) Restore() error {
	prev := filepath.Join(t.Dir, previousDir)
	if _, err := os.Stat(prev); err != nil {
		return fmt.Errorf("no previous source to restore: %w", err)
	}
	cur := filepath.Join(t.Dir, currentDir)
	if err := os.RemoveAll(cur); err != nil {
		return err
	}
	return os.Rename(prev, cur)
}

// Version reads the release a source directory under Dir holds (nix/version.nix).
func (t *Tree) Version(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(t.Dir, dir, "nix", "version.nix"))
	if err != nil {
		return "", err
	}
	return strings.Trim(strings.TrimSpace(string(b)), `"`), nil
}

// unpack extracts a tarball, gzipped or not, into dest. With top set, every entry must sit
// under top/ (git archive's prefix), which is stripped.
func unpack(tarball, top, dest string) error {
	f, err := os.Open(tarball)
	if err != nil {
		return err
	}
	defer f.Close()
	br := bufio.NewReader(f)
	var r io.Reader = br
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return err
		}
		r = gz
	}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader { // git archive's commit header
			continue
		}
		rel, ok := entryPath(hdr.Name, top)
		if !ok {
			return fmt.Errorf("tarball entry %q is outside %s/", hdr.Name, top)
		}
		if err := extract(tr, hdr, filepath.Join(dest, rel)); err != nil {
			return err
		}
	}
}

// entryPath is name relative to top, refusing absolute paths and any "..".
func entryPath(name, top string) (string, bool) {
	clean := filepath.Clean(name)
	if filepath.IsAbs(clean) || strings.Contains(clean, "..") {
		return "", false
	}
	if top == "" {
		return clean, true
	}
	if clean == top {
		return "", true
	}
	rel, ok := strings.CutPrefix(clean, top+"/")
	return rel, ok
}

func extract(r io.Reader, hdr *tar.Header, path string) error {
	switch hdr.Typeflag {
	case tar.TypeDir:
		return os.MkdirAll(path, 0o755)
	case tar.TypeSymlink:
		if filepath.IsAbs(hdr.Linkname) || strings.Contains(hdr.Linkname, "..") {
			return fmt.Errorf("tarball symlink %q -> %q leaves the tree", hdr.Name, hdr.Linkname)
		}
		return os.Symlink(hdr.Linkname, path)
	case tar.TypeReg:
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode&0o755|0o644))
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, r); err != nil {
			_ = out.Close()
			return err
		}
		return out.Close()
	}
	return fmt.Errorf("tarball entry %q has unsupported type %c", hdr.Name, hdr.Typeflag)
}
