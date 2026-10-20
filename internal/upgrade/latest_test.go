package upgrade

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchLatestReadsTheTag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v1.12.0","name":"Expanse 1.12.0"}`))
	}))
	defer srv.Close()
	got, err := FetchLatest(context.Background(), srv.Client(), srv.URL)
	if err != nil || got != "1.12.0" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestFetchLatestRejectsATagThatIsNotAVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"nightly"}`))
	}))
	defer srv.Close()
	if _, err := FetchLatest(context.Background(), srv.Client(), srv.URL); err == nil {
		t.Fatal("accepted a non-version tag")
	}
}
