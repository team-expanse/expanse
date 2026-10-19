package sshkeys

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func fakeGitHub(t *testing.T) string {
	t.Helper()
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/octo.keys":
			_, _ = w.Write([]byte("ssh-ed25519 AAAAone\n\nssh-rsa AAAAtwo\n"))
		case "/empty.keys":
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(gh.Close)
	return gh.URL
}

func TestExpandReplacesGitHubEntriesWithTheirKeys(t *testing.T) {
	got, err := Expand(context.Background(), fakeGitHub(t), []string{"ssh-ed25519 AAAAown me", "gh:octo"})
	want := []string{"ssh-ed25519 AAAAown me", "ssh-ed25519 AAAAone", "ssh-rsa AAAAtwo"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Expand = %v, %v; want %v", got, err, want)
	}
}

func TestExpandRejectsUnusableGitHubEntries(t *testing.T) {
	base := fakeGitHub(t)
	for user, msg := range map[string]string{
		"nobody":   "404",
		"empty":    "no public SSH keys",
		"../etc":   "not a GitHub user name",
		"-leading": "not a GitHub user name",
	} {
		if _, err := Expand(context.Background(), base, []string{"gh:" + user}); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("gh:%s: err = %v, want it to mention %q", user, err, msg)
		}
	}
}
