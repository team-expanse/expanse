package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/expanse/expanse/internal/blocks/vm"
	"github.com/expanse/expanse/internal/web/schemaform"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
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

func TestVMDetailShowsTheAddressesTheGuestAgentReports(t *testing.T) {
	srv, pw, st := newTestServerStore(t)
	seedNode(t, st, "n1", "kvm")
	c, csrf := loggedInClient(t, srv, pw)
	if resp, body := postForm(t, c, srv.URL+"/deploy/vm/instance", csrf, url.Values{
		"name": {"vm1"}, "cfg/image" + schemaform.PresetSuffix: {"Debian 13 (log in as debian)"},
	}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("deploy: %d\n%s", resp.StatusCode, alerts(body))
	}
	_, body := get(t, c, srv.URL+"/blocks/default/vm1")
	if !strings.Contains(body, "No guest agent has answered yet") {
		t.Error("the page does not say the address is not known yet")
	}
	status, _ := proto.Marshal(&pb.BlockStatus{Placements: []*pb.PlacementStatus{
		{ReplicaIndex: -1, FormerIndex: 0, NodeId: "n2", Phase: pb.Phase_FAILED},
		{ReplicaIndex: 0, NodeId: "n1", Phase: pb.Phase_RUNNING},
	}})
	ctx := context.Background()
	if _, err := st.Put(ctx, "/blocks/default/vm1/status", status); err != nil {
		t.Fatal(err)
	}
	rec := `health=healthy in_sync=true ready=true actions=0 error="" updated=1 status="ready: multi-user.target reached (addresses: 192.168.1.50 fd00::5)"`
	if _, err := st.Put(ctx, "/node/n1/status/resources/block-replica:default/vm1/0", []byte(rec)); err != nil {
		t.Fatal(err)
	}
	_, body = get(t, c, srv.URL+"/blocks/default/vm1")
	for _, want := range []string{"192.168.1.50", "fd00::5", "ssh debian@192.168.1.50", "ready: multi-user.target reached"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
}

func TestVMDeployWithThePandoSetup(t *testing.T) {
	const pando = "Pando 0.12.0 under Docker (Debian 13 or Ubuntu 24.04, 40 GiB disk or more)"
	srv, pw, st := newTestServerStore(t)
	seedNode(t, st, "n1", "kvm")
	c, csrf := loggedInClient(t, srv, pw)
	if _, body := get(t, c, srv.URL+"/deploy/vm/instance"); !strings.Contains(body, pando) {
		t.Fatal("the form does not offer the Pando setup")
	}
	resp, body := postForm(t, c, srv.URL+"/deploy/vm/instance", csrf, url.Values{
		"name": {"pando"}, "cfg/image" + schemaform.PresetSuffix: {"Debian 13 (log in as debian)"},
		"cfg/cloudInit/sshKeys": {"ssh-ed25519 AAAAown"}, "cfg/cloudInit/setup" + schemaform.PresetSuffix: {pando},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status %d:\n%s", resp.StatusCode, alerts(body))
	}
	setup := getBlock(t, st, "default/pando").GetSpec().GetConfig().AsMap()["cloudInit"].(map[string]any)["setup"].(map[string]any)
	if pk := setup["packages"].([]any); pk[0] != "docker.io" || len(setup["runcmd"].([]any)) == 0 {
		t.Errorf("setup = %v", setup)
	}
}
