package vm

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImageFromConfig(t *testing.T) {
	if im, err := ImageFromConfig(map[string]any{}); im != nil || err != nil {
		t.Fatalf("no image: %v, %v", im, err)
	}
	im, err := ImageFromConfig(map[string]any{"image": map[string]any{"url": "https://x/a.qcow2", "checksumUrl": "https://x/SUMS"}})
	if err != nil || im.URL != "https://x/a.qcow2" || im.ChecksumURL != "https://x/SUMS" {
		t.Fatalf("image = %+v, %v", im, err)
	}
	for name, cfg := range map[string]any{
		"no url":      map[string]any{"checksum": strings.Repeat("a", 64)},
		"no checksum": map[string]any{"url": "https://x/a.qcow2"},
		"bad digest":  map[string]any{"url": "https://x/a.qcow2", "checksum": "abc"},
		"not https":   map[string]any{"url": "file:///etc/shadow", "checksum": strings.Repeat("a", 64)},
	} {
		if _, err := ImageFromConfig(map[string]any{"image": cfg}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSumsLookup(t *testing.T) {
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 128)
	sums := a + "  other.qcow2\n" +
		b + " *debian-13-genericcloud-amd64.qcow2\n" +
		"SHA256 (Fedora-Cloud.qcow2) = " + a + "\n"
	for file, want := range map[string]string{
		"debian-13-genericcloud-amd64.qcow2": b,
		"other.qcow2":                        a,
		"Fedora-Cloud.qcow2":                 a,
	} {
		if got, err := sumsLookup([]byte(sums), file); err != nil || got != want {
			t.Errorf("%s: %q, %v", file, got, err)
		}
	}
	if _, err := sumsLookup([]byte(sums), "missing.qcow2"); err == nil {
		t.Error("found a file the sums do not list")
	}
}

func serve(t *testing.T, files map[string][]byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchVerifiesAndCaches(t *testing.T) {
	img := []byte("pretend this is a qcow2")
	s256 := sha256.Sum256(img)
	s512 := sha512.Sum512(img)
	srv := serve(t, map[string][]byte{
		"/img.qcow2":  img,
		"/SHA512SUMS": []byte(hex.EncodeToString(s512[:]) + "  img.qcow2\n"),
	})
	dir := t.TempDir()
	ctx := context.Background()

	path, err := Fetch(ctx, srv.Client(), Image{URL: srv.URL + "/img.qcow2", ChecksumURL: srv.URL + "/SHA512SUMS"}, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != string(img) {
		t.Fatalf("cached %q", got)
	}
	if filepath.Dir(path) != dir {
		t.Errorf("cached outside the cache dir: %s", path)
	}

	// The same digest again comes from the cache: the server is gone.
	srv.Close()
	again, err := Fetch(ctx, nil, Image{URL: "http://gone/img.qcow2", Checksum: hex.EncodeToString(s512[:])}, dir, nil)
	if err != nil || again != path {
		t.Fatalf("cache miss: %s, %v", again, err)
	}
	if _, err := Fetch(ctx, nil, Image{URL: "http://gone/img.qcow2", Checksum: hex.EncodeToString(s256[:])}, dir, nil); err == nil {
		t.Error("a sha256 of the same file was served from the sha512 cache entry without downloading")
	}
}

func TestFetchRefusesAMismatch(t *testing.T) {
	srv := serve(t, map[string][]byte{"/img.qcow2": []byte("tampered")})
	dir := t.TempDir()
	_, err := Fetch(context.Background(), srv.Client(), Image{URL: srv.URL + "/img.qcow2", Checksum: strings.Repeat("0", 64)}, dir, nil)
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("err = %v", err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("a failed download left %d files in the cache", len(ents))
	}
}

func TestFetchReportsProgress(t *testing.T) {
	img := []byte(strings.Repeat("x", 1000))
	sum := sha256.Sum256(img)
	srv := serve(t, map[string][]byte{"/img.raw": img})
	var last, total int64
	_, err := Fetch(context.Background(), srv.Client(), Image{URL: srv.URL + "/img.raw", Checksum: hex.EncodeToString(sum[:])}, t.TempDir(),
		func(n, of int64) { last, total = n, of })
	if err != nil || last != 1000 || total != 1000 {
		t.Fatalf("progress %d/%d, %v", last, total, err)
	}
}
