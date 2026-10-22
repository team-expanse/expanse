package upgrade

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func serveReleases(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchReleasesListsPublishedVersionsNewestFirst(t *testing.T) {
	srv := serveReleases(t, `[
		{"tag_name":"v1.9.0"},
		{"tag_name":"v1.15.0"},
		{"tag_name":"v1.16.0","draft":true},
		{"tag_name":"v1.16.0-rc1","prerelease":true},
		{"tag_name":"nightly"},
		{"tag_name":"v1.14.2"}]`)
	got, err := FetchReleases(context.Background(), srv.Client(), srv.URL)
	if want := []string{"1.15.0", "1.14.2", "1.9.0"}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("got %q, %v; want %q", got, err, want)
	}
}

func TestFetchReleasesFailsWithNoRelease(t *testing.T) {
	srv := serveReleases(t, `[{"tag_name":"nightly"}]`)
	if _, err := FetchReleases(context.Background(), srv.Client(), srv.URL); err == nil {
		t.Fatal("no error without a release")
	}
}

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{{"1.15.0", "1.9.0", true}, {"1.9.0", "1.15.0", false}, {"1.15.0", "1.15.0", false}, {"1.15.0", "", true}, {"", "1.15.0", false}} {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v", c.a, c.b, got)
		}
	}
}
