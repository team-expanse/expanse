package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/expanse/expanse/internal/blocks/vm"
	"github.com/expanse/expanse/internal/web/schemaform"
)

const debian13 = "https://cloud.debian.org/images/cloud/trixie/latest/debian-13-genericcloud-amd64.qcow2"

// fakeGitHub serves /<user>.keys for octo only.
func fakeGitHub(t *testing.T) *httptest.Server {
	t.Helper()
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/octo.keys" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("ssh-ed25519 AAAAgh1 \nssh-rsa AAAAgh2\n"))
	}))
	t.Cleanup(gh.Close)
	return gh
}

func TestVMFormOffersImagesAndStartsFromDebian(t *testing.T) {
	srv, pw, st := newTestServerStore(t)
	seedNode(t, st, "n1", "kvm")
	c, _ := loggedInClient(t, srv, pw)
	_, body := get(t, c, srv.URL+"/deploy/vm/instance")
	for _, want := range []string{`name="cfg/image@preset"`, "Debian 13 (log in as debian)", "Ubuntu 24.04 LTS", `value="custom"`, `name="cfg/cloudInit/sshKeys"`} {
		if !strings.Contains(body, want) {
			t.Errorf("form lacks %q", want)
		}
	}
	if !strings.Contains(body, `selected>Debian 13`) {
		t.Error("Debian 13 is not the starting choice")
	}
}

func TestVMDeployImportsGitHubKeys(t *testing.T) {
	gh := fakeGitHub(t)
	srv, pw, st := newTestServerWith(t, func(s *Server) { s.GitHubURL = gh.URL })
	seedNode(t, st, "n1", "kvm")
	c, csrf := loggedInClient(t, srv, pw)
	resp, body := postForm(t, c, srv.URL+"/deploy/vm/instance", csrf, url.Values{
		"name": {"vm1"}, "cfg/image" + schemaform.PresetSuffix: {"Debian 13 (log in as debian)"},
		"cfg/cloudInit/sshKeys": {"ssh-ed25519 AAAAown me@laptop\ngh:octo"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status %d:\n%s", resp.StatusCode, alerts(body))
	}
	cfg := getBlock(t, st, "default/vm1").GetSpec().GetConfig().AsMap()
	if img := cfg["image"].(map[string]any); img["url"] != debian13 {
		t.Errorf("image = %v", img)
	}
	keys := cfg["cloudInit"].(map[string]any)["sshKeys"].([]any)
	want := []any{"ssh-ed25519 AAAAown me@laptop", "ssh-ed25519 AAAAgh1", "ssh-rsa AAAAgh2"}
	if len(keys) != len(want) || keys[0] != want[0] || keys[1] != want[1] || keys[2] != want[2] {
		t.Errorf("keys = %q, want %q", keys, want)
	}
}

func TestVMDeployReportsAnUnknownGitHubUser(t *testing.T) {
	gh := fakeGitHub(t)
	srv, pw, st := newTestServerWith(t, func(s *Server) { s.GitHubURL = gh.URL })
	seedNode(t, st, "n1", "kvm")
	c, csrf := loggedInClient(t, srv, pw)
	resp, body := postForm(t, c, srv.URL+"/deploy/vm/instance", csrf, url.Values{
		"name": {"vm1"}, "cfg/image" + schemaform.PresetSuffix: {"Debian 13 (log in as debian)"},
		"cfg/cloudInit/sshKeys": {"gh:nobody"},
	})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "nobody") {
		t.Fatalf("status %d:\n%s", resp.StatusCode, alerts(body))
	}
}

func TestVMDetailShowsTheGuestsMAC(t *testing.T) {
	srv, pw, st := newTestServerStore(t)
	seedNode(t, st, "n1", "kvm")
	c, csrf := loggedInClient(t, srv, pw)
	if resp, body := postForm(t, c, srv.URL+"/deploy/vm/instance", csrf, url.Values{
		"name": {"vm1"}, "cfg/image" + schemaform.PresetSuffix: {"Debian 13 (log in as debian)"},
		"cfg/cloudInit/sshKeys": {"ssh-ed25519 AAAAown"},
	}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("deploy: %d\n%s", resp.StatusCode, alerts(body))
	}
	_, body := get(t, c, srv.URL+"/blocks/default/vm1")
	if mac := vm.DefaultMAC("default-vm1-0"); !strings.Contains(body, mac) {
		t.Errorf("the page lacks the guest's MAC %s", mac)
	}
	if !strings.Contains(body, "ssh debian@vm1") {
		t.Error("the page does not say how to log in")
	}
}
