package upgrade

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testRelease serves one release's assets the way GitHub's download URLs do.
type testRelease struct {
	srv    *httptest.Server
	pub    string
	assets map[string][]byte
}

func newTestRelease(t *testing.T, version string, files map[string][]byte) *testRelease {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var sums strings.Builder
	for name, body := range files {
		h := sha256.Sum256(body)
		sums.WriteString(hex.EncodeToString(h[:]) + "  " + name + "\n")
	}
	r := &testRelease{
		pub:    "expanse-test-1:" + base64.StdEncoding.EncodeToString(pub),
		assets: map[string][]byte{"SHA256SUMS": []byte(sums.String())},
	}
	r.assets["SHA256SUMS.sig"] = []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, r.assets["SHA256SUMS"])) + "\n")
	for name, body := range files {
		r.assets[name] = body
	}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		name := strings.TrimPrefix(req.URL.Path, "/v"+version+"/")
		body, ok := r.assets[name]
		if !ok {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *testRelease) source(t *testing.T) *Source {
	t.Helper()
	return &Source{BaseURL: r.srv.URL, PublicKey: r.pub, Client: r.srv.Client(), CacheDir: t.TempDir()}
}

func TestFetchVerifiesTheTarballAgainstSignedSums(t *testing.T) {
	r := newTestRelease(t, "1.12.0", map[string][]byte{"expanse-1.12.0.tar.gz": []byte("tarball")})
	got, err := r.source(t).Fetch(context.Background(), "1.12.0")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(got.Tarball)
	if string(body) != "tarball" {
		t.Errorf("tarball = %q", body)
	}
	if got.Cache != "" {
		t.Errorf("cache = %q for a release without one", got.Cache)
	}
}

func TestFetchRefusesATamperedTarball(t *testing.T) {
	r := newTestRelease(t, "1.12.0", map[string][]byte{"expanse-1.12.0.tar.gz": []byte("tarball")})
	r.assets["expanse-1.12.0.tar.gz"] = []byte("evil")
	if _, err := r.source(t).Fetch(context.Background(), "1.12.0"); err == nil {
		t.Fatal("a tarball that does not match SHA256SUMS was accepted")
	}
}

func TestFetchRefusesSumsSignedByAnotherKey(t *testing.T) {
	r := newTestRelease(t, "1.12.0", map[string][]byte{"expanse-1.12.0.tar.gz": []byte("tarball")})
	other := newTestRelease(t, "1.12.0", nil)
	s := r.source(t)
	s.PublicKey = other.pub
	if _, err := s.Fetch(context.Background(), "1.12.0"); err == nil {
		t.Fatal("SHA256SUMS signed by an unknown key was accepted")
	}
}

func TestFetchTakesTheCacheForThisSystem(t *testing.T) {
	name := "expanse-1.12.0-cache-" + System() + ".tar"
	r := newTestRelease(t, "1.12.0", map[string][]byte{"expanse-1.12.0.tar.gz": []byte("tarball"), name: []byte("cache")})
	got, err := r.source(t).Fetch(context.Background(), "1.12.0")
	if err != nil {
		t.Fatal(err)
	}
	if body, _ := os.ReadFile(got.Cache); string(body) != "cache" {
		t.Errorf("cache = %q", body)
	}
	if filepath.Dir(got.Cache) != filepath.Dir(got.Tarball) {
		t.Errorf("cache %s and tarball %s are kept apart", got.Cache, got.Tarball)
	}
}

func TestFetchRejectsAVersionThatIsNotOne(t *testing.T) {
	r := newTestRelease(t, "1.12.0", nil)
	for _, v := range []string{"", "../1.12.0", "1.12.0/x", "latest"} {
		if _, err := r.source(t).Fetch(context.Background(), v); err == nil {
			t.Errorf("version %q accepted", v)
		}
	}
}
