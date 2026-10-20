// Package upgrade moves a node to another Expanse release: fetch and verify the release's
// assets, replace the node's source copy, build and switch.
package upgrade

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// DefaultBaseURL is where GitHub serves release assets, one directory per tag.
const DefaultBaseURL = "https://github.com/team-expanse/expanse/releases/download"

// Source downloads release assets and checks them against the release's signed SHA256SUMS.
type Source struct {
	BaseURL string
	// PublicKey verifies SHA256SUMS.sig, in Nix's "name:base64" form (it also signs the cache).
	PublicKey string
	Client    *http.Client
	// CacheDir keeps downloads, one directory per version.
	CacheDir string
}

// Fetched is a verified release on disk. Cache is empty when the release has none for this system.
type Fetched struct {
	Version string
	Tarball string
	Cache   string
}

var versionRE = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$`)

// System is this machine's Nix system name, which names its binary cache asset.
func System() string {
	arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
	return arch + "-linux"
}

// TarballName and CacheName are the asset names release.sh publishes.
func TarballName(v string) string { return "expanse-" + v + ".tar.gz" }

// CacheName is the binary cache asset for a version on this system.
func CacheName(v string) string { return "expanse-" + v + "-cache-" + System() + ".tar" }

// Fetch downloads a version's source tarball, and its cache when published, verifying both.
func (s *Source) Fetch(ctx context.Context, version string) (*Fetched, error) {
	if !versionRE.MatchString(version) {
		return nil, fmt.Errorf("%q is not a release version", version)
	}
	sums, err := s.get(ctx, version, "SHA256SUMS")
	if err != nil {
		return nil, err
	}
	sig, err := s.get(ctx, version, "SHA256SUMS.sig")
	if err != nil {
		return nil, err
	}
	if err := verifySignature(s.PublicKey, sums, sig); err != nil {
		return nil, err
	}
	want, err := parseSums(sums)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(s.CacheDir, version)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	out := &Fetched{Version: version}
	if out.Tarball, err = s.download(ctx, version, TarballName(version), dir, want); err != nil {
		return nil, err
	}
	if _, ok := want[CacheName(version)]; ok {
		if out.Cache, err = s.download(ctx, version, CacheName(version), dir, want); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Prune removes every release's downloads from CacheDir except keep's ("" keeps none).
func (s *Source) Prune(keep string) error {
	ents, err := os.ReadDir(s.CacheDir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, e := range ents {
		if e.IsDir() && e.Name() != keep && versionRE.MatchString(e.Name()) {
			if err := os.RemoveAll(filepath.Join(s.CacheDir, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Source) url(version, name string) string {
	base := s.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	return base + "/v" + version + "/" + name
}

func (s *Source) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return http.DefaultClient
}

func (s *Source) open(ctx context.Context, version, name string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url(version, name), nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", name, err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("download %s: %s", name, resp.Status)
	}
	return resp.Body, nil
}

// get reads a small asset into memory.
func (s *Source) get(ctx context.Context, version, name string) ([]byte, error) {
	body, err := s.open(ctx, version, name)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return io.ReadAll(io.LimitReader(body, 1<<20))
}

// download streams an asset to dir, keeping it only when its digest matches SHA256SUMS.
func (s *Source) download(ctx context.Context, version, name, dir string, want map[string]string) (string, error) {
	digest, ok := want[name]
	if !ok {
		return "", fmt.Errorf("SHA256SUMS does not list %s", name)
	}
	body, err := s.open(ctx, version, name)
	if err != nil {
		return "", err
	}
	defer body.Close()
	tmp, err := os.CreateTemp(dir, name+".part-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), body); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("download %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != digest {
		return "", fmt.Errorf("%s: sha256 %s, SHA256SUMS says %s", name, got, digest)
	}
	path := filepath.Join(dir, name)
	return path, os.Rename(tmp.Name(), path)
}

// verifySignature checks sig (base64 ed25519) over sums with a "name:base64" public key.
func verifySignature(publicKey string, sums, sig []byte) error {
	_, b64, ok := strings.Cut(publicKey, ":")
	if !ok {
		return fmt.Errorf("release public key %q is not name:base64", publicKey)
	}
	pub, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("release public key %q is not an ed25519 key", publicKey)
	}
	raw, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(sig)))
	if err != nil || !ed25519.Verify(pub, sums, raw) {
		return fmt.Errorf("SHA256SUMS is not signed by %s", strings.SplitN(publicKey, ":", 2)[0])
	}
	return nil
}

// parseSums reads sha256sum output: "<hex>  <name>" per line.
func parseSums(sums []byte) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		digest, name, ok := strings.Cut(sc.Text(), "  ")
		if !ok || len(digest) != 64 {
			return nil, fmt.Errorf("bad SHA256SUMS line %q", sc.Text())
		}
		out[strings.TrimPrefix(name, "*")] = digest
	}
	return out, sc.Err()
}

// ValidVersion reports whether v names a release (1.12.0, 1.12.0-rc.1).
func ValidVersion(v string) bool { return versionRE.MatchString(v) }

// Sign signs sums with a Nix-format secret key ("name:base64" of the 64-byte ed25519 key, as
// `nix key generate-secret` writes it), returning the base64 signature SHA256SUMS.sig holds.
func Sign(secretKey string, sums []byte) ([]byte, error) {
	_, b64, ok := strings.Cut(strings.TrimSpace(secretKey), ":")
	if !ok {
		return nil, fmt.Errorf("secret key is not name:base64")
	}
	priv, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("secret key is not an ed25519 key")
	}
	return []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, sums)) + "\n"), nil
}
