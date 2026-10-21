package web

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
)

// seedNode records a cluster node and the capabilities it advertises.
func seedNode(t *testing.T, st store.Store, id, caps string) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.Put(ctx, store.Key("/nodes/"+id), []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, store.Key("/nodes/"+id+"/capabilities"), []byte(caps)); err != nil {
		t.Fatal(err)
	}
}

func get(t *testing.T, c *http.Client, u string) (int, string) {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func postForm(t *testing.T, c *http.Client, u, csrf string, form url.Values) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, u, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(csrfHeader, csrf)
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	defer func() { c.CheckRedirect = nil }()
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", u, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func TestDeployCatalogListsEveryTypeAndWhatItNeeds(t *testing.T) {
	srv, pw, st := newTestServerStore(t)
	seedNode(t, st, "n1", "aes-ni")
	c, _ := loggedInClient(t, srv, pw)
	code, body := get(t, c, srv.URL+"/deploy")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	for _, id := range []string{"web/nginx", "db/postgres", "vm/instance"} {
		if !strings.Contains(body, `href="/deploy/`+id+`"`) {
			t.Errorf("no link to /deploy/%s", id)
		}
	}
	if !strings.Contains(body, "No node has kvm") {
		t.Error("vm/instance is not marked as needing kvm, which no node has")
	}
	if !strings.Contains(body, `href="/blocks/new"`) {
		t.Error("no link to the YAML deploy page")
	}
}

func TestDeployFormStartsFromTheTemplate(t *testing.T) {
	srv, pw, st := newTestServerStore(t)
	seedNode(t, st, "n1", "kvm")
	c, _ := loggedInClient(t, srv, pw)

	_, body := get(t, c, srv.URL+"/deploy/web/nginx")
	for _, want := range []string{
		`name="cfg/serverName"`, `name="cfg/port"`, `value="8080"`, // config from schema + defaults
		`name="replicas"`, `name="name" value="nginx"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("nginx form lacks %s", want)
		}
	}
	if !strings.Contains(body, `name="replicas" value="1"`) {
		t.Error("replicas not capped at the cluster's one node (template says 3)")
	}

	_, body = get(t, c, srv.URL+"/deploy/vm/instance")
	if strings.Contains(body, `name="replicas"`) {
		t.Error("a SINGLETON type offers a replica count")
	}
	if !strings.Contains(body, `name="storage/disk/size" value="20Gi"`) {
		t.Error("vm/instance form lacks its disk size from the template")
	}

	if code, _ := get(t, c, srv.URL+"/deploy/no/such"); code != http.StatusNotFound {
		t.Errorf("unknown type: status %d, want 404", code)
	}
}

func TestDeployFormCreatesTheBlock(t *testing.T) {
	srv, pw, st := newTestServerStore(t)
	seedNode(t, st, "n1", "kvm")
	c, csrf := loggedInClient(t, srv, pw)

	resp, body := postForm(t, c, srv.URL+"/deploy/web/nginx", csrf, url.Values{
		"name": {"site"}, "replicas": {"1"}, "cpu": {"250m"}, "memory": {"128Mi"},
		"cfg/serverName": {"www.example.org"}, "cfg/port": {"8080"},
	})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/blocks/default/site" {
		t.Fatalf("status %d, Location %q:\n%s", resp.StatusCode, resp.Header.Get("Location"), body)
	}
	b := getBlock(t, st, "default/site")
	s := b.GetSpec()
	if s.GetType() != "web/nginx" || s.GetReplicas() != 1 || s.GetResources().GetRequests().GetCpu() != "250m" {
		t.Errorf("spec = %v", s)
	}
	if p := s.GetNetwork().GetPorts(); len(p) != 1 || p[0].GetTargetPort() != 8080 {
		t.Errorf("ports not from the template: %v", p)
	}
	if cfg := s.GetConfig().AsMap(); cfg["serverName"] != "www.example.org" || cfg["port"] != float64(8080) {
		t.Errorf("config = %v", cfg)
	}

	resp, body = postForm(t, c, srv.URL+"/deploy/vm/instance", csrf, url.Values{
		"name": {"vm1"}, "cpu": {"4"}, "memory": {"8Gi"}, "storage/disk/size": {"50Gi"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("vm deploy status %d:\n%s", resp.StatusCode, alerts(body))
	}
	disk := getBlock(t, st, "default/vm1").GetSpec().GetStorage()[0]
	if disk.GetSize() != "50Gi" || disk.GetReplication() != 1 {
		t.Errorf("disk = %v, want 50Gi kept on the one node", disk)
	}
}

func TestDeployFormShowsEveryErrorOnItsField(t *testing.T) {
	srv, pw, st := newTestServerStore(t)
	seedNode(t, st, "n1", "")
	c, csrf := loggedInClient(t, srv, pw)

	resp, body := postForm(t, c, srv.URL+"/deploy/db/postgres", csrf, url.Values{
		"name": {"Bad_Name"}, "replicas": {"1"}, "cfg/port": {"eighty"}, "cfg/superuserPassword": {"typed-secret-9"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
	if strings.Contains(body, "typed-secret-9") || resp.Header.Get("Cache-Control") != "no-store" {
		t.Error("the rejected form echoes a typed secret or may be cached")
	}
	for _, field := range []string{"name", "cfg/port"} {
		if !strings.Contains(body, `name="`+field+`"`) || !invalid(body, field) {
			t.Errorf("field %s is not marked invalid; errors:\n%s", field, alerts(body))
		}
	}
	if !strings.Contains(body, `value="eighty"`) {
		t.Error("the rejected input was not kept for correcting")
	}
	if _, err := st.Get(context.Background(), store.Key("/blocks/default/Bad_Name")); err == nil {
		t.Error("an invalid block was created")
	}
}

// invalid reports whether the input named field carries aria-invalid.
func invalid(body, field string) bool {
	i := strings.Index(body, `name="`+field+`"`)
	if i < 0 {
		return false
	}
	end := strings.Index(body[i:], ">")
	return end > 0 && strings.Contains(body[i:i+end], `aria-invalid="true"`)
}

func getBlock(t *testing.T, st store.Store, full string) *pb.Block {
	t.Helper()
	e, err := st.Get(context.Background(), store.Key("/blocks/"+full))
	if err != nil {
		t.Fatalf("block %s: %v", full, err)
	}
	b := &pb.Block{}
	if err := proto.Unmarshal(e.Value, b); err != nil {
		t.Fatal(err)
	}
	return b
}

// alerts returns the page's error messages, for failure output.
func alerts(body string) string {
	var out []string
	for _, part := range strings.Split(body, `class="field-error"`)[1:] {
		for _, closer := range []string{"</div>", "</span>"} {
			if end := strings.Index(part, closer); end >= 0 {
				part = part[:end]
			}
		}
		out = append(out, strings.TrimSpace(tagRE.ReplaceAllString(part, "")))
	}
	return strings.Join(out, "\n")
}

var tagRE = regexp.MustCompile(`<[^>]*>|^[^>]*>`)

// Changing a config port moves the template's forwarding and health check with it.
func TestDeployFormPortsFollowTheConfig(t *testing.T) {
	srv, pw, st := newTestServerStore(t)
	seedNode(t, st, "n1", "")
	c, csrf := loggedInClient(t, srv, pw)
	resp, body := postForm(t, c, srv.URL+"/deploy/web/caddy", csrf, url.Values{
		"name": {"proxy"}, "cfg/port": {"18090"}, "cfg/httpsPort": {"18453"},
		"cfg/sites": {"- host: app.example.org\n  respond: ok\n"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status %d:\n%s", resp.StatusCode, alerts(body))
	}
	spec := getBlock(t, st, "default/proxy").GetSpec()
	got := map[string]int32{}
	for _, p := range spec.GetNetwork().GetPorts() {
		got[p.GetName()] = p.GetTargetPort()
	}
	if got["http"] != 18090 || got["https"] != 18453 {
		t.Errorf("port targets = %v, want http→18090 https→18453", got)
	}
	if p := spec.GetNetwork().GetHealthCheck().GetReadiness().GetPort(); p != 18090 {
		t.Errorf("readiness port = %d, want 18090", p)
	}
}

func deployNginx(t *testing.T, c *http.Client, base, csrf string) {
	t.Helper()
	resp, body := postForm(t, c, base+"/deploy/web/nginx", csrf, url.Values{
		"name": {"site"}, "replicas": {"1"}, "cfg/serverName": {"www.example.org"}, "cfg/port": {"8080"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("deploy: status %d:\n%s", resp.StatusCode, alerts(body))
	}
}

func TestEditFormShowsTheBlock(t *testing.T) {
	srv, pw, st := newTestServerStore(t)
	seedNode(t, st, "n1", "")
	c, csrf := loggedInClient(t, srv, pw)
	deployNginx(t, c, srv.URL, csrf)

	_, detail := get(t, c, srv.URL+"/blocks/default/site")
	if !strings.Contains(detail, `href="/blocks/default/site/edit"`) {
		t.Error("the detail page has no Edit link")
	}
	code, body := get(t, c, srv.URL+"/blocks/default/site/edit")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	for _, want := range []string{`value="www.example.org"`, `action="/blocks/default/site/edit"`, `name="replicas" value="1"`} {
		if !strings.Contains(body, want) {
			t.Errorf("edit form lacks %s", want)
		}
	}
	if !strings.Contains(body, `name="name" value="site" readonly`) {
		t.Error("the name is editable")
	}
	if code, _ := get(t, c, srv.URL+"/blocks/default/nope/edit"); code != http.StatusNotFound {
		t.Errorf("missing block: status %d, want 404", code)
	}
}

func TestEditFormUpdatesTheBlock(t *testing.T) {
	srv, pw, st := newTestServerStore(t)
	seedNode(t, st, "n1", "")
	c, csrf := loggedInClient(t, srv, pw)
	deployNginx(t, c, srv.URL, csrf)

	resp, body := postForm(t, c, srv.URL+"/blocks/default/site/edit", csrf, url.Values{
		"name": {"site"}, "replicas": {"1"}, "cpu": {"300m"}, "memory": {"96Mi"},
		"cfg/serverName": {"new.example.org"}, "cfg/port": {"8090"}, "cfg/root": {"/var/www"},
	})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/blocks/default/site" {
		t.Fatalf("status %d:\n%s", resp.StatusCode, alerts(body))
	}
	s := getBlock(t, st, "default/site").GetSpec()
	if cfg := s.GetConfig().AsMap(); cfg["serverName"] != "new.example.org" || cfg["port"] != float64(8090) {
		t.Errorf("config = %v", cfg)
	}
	if s.GetResources().GetRequests().GetCpu() != "300m" {
		t.Errorf("cpu = %q", s.GetResources().GetRequests().GetCpu())
	}
	if p := s.GetNetwork().GetPorts()[0]; p.GetTargetPort() != 8090 || p.GetPort() != 80 {
		t.Errorf("port = %v, want 80 → 8090", p)
	}

	resp, body = postForm(t, c, srv.URL+"/blocks/default/site/edit", csrf, url.Values{
		"name": {"site"}, "replicas": {"1"}, "cfg/serverName": {"new.example.org"}, "cfg/port": {"0"},
	})
	if resp.StatusCode != http.StatusBadRequest || !invalid(body, "cfg/port") {
		t.Errorf("a bad port: status %d, errors:\n%s", resp.StatusCode, alerts(body))
	}
}

// A second block of a type gets the next free port, so two can share a node.
func TestDeployFormPicksAFreePort(t *testing.T) {
	srv, pw, st := newTestServerStore(t)
	seedNode(t, st, "n1", "")
	c, csrf := loggedInClient(t, srv, pw)
	deployNginx(t, c, srv.URL, csrf) // config port 8080

	_, body := get(t, c, srv.URL+"/deploy/web/nginx")
	if !strings.Contains(body, `name="cfg/port" value="8081"`) {
		t.Error("the form does not offer the next free port, 8081")
	}
	resp, body := postForm(t, c, srv.URL+"/deploy/web/nginx", csrf, url.Values{
		"name": {"site2"}, "cfg/serverName": {"two.example.org"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status %d:\n%s", resp.StatusCode, alerts(body))
	}
	s := getBlock(t, st, "default/site2").GetSpec()
	if s.GetConfig().AsMap()["port"] != float64(8081) || s.GetNetwork().GetPorts()[0].GetTargetPort() != 8081 {
		t.Errorf("second nginx: config port %v, target %d; want 8081 for both",
			s.GetConfig().AsMap()["port"], s.GetNetwork().GetPorts()[0].GetTargetPort())
	}
}

func TestDeployFormSetsTheAddressPort(t *testing.T) {
	srv, pw, st := newTestServerStore(t)
	seedNode(t, st, "n1", "")
	c, csrf := loggedInClient(t, srv, pw)

	_, body := get(t, c, srv.URL+"/deploy/web/nginx")
	for _, want := range []string{`name="port/http" value="80"`, "behind the block&#39;s address"} {
		if !strings.Contains(body, want) {
			t.Errorf("nginx form lacks %s", want)
		}
	}

	_, body = postForm(t, c, srv.URL+"/deploy/web/nginx", csrf, url.Values{
		"name": {"site"}, "cfg/serverName": {"www.example.org"}, "port/http": {"70000"},
	})
	if !strings.Contains(body, "Port http must be a number from 1 to 65535") {
		t.Errorf("out-of-range address port accepted:\n%s", alerts(body))
	}
	if !strings.Contains(body, `name="port/http" value="70000"`) {
		t.Error("the rejected address port was replaced instead of kept for correcting")
	}

	resp, body := postForm(t, c, srv.URL+"/deploy/web/nginx", csrf, url.Values{
		"name": {"site"}, "cfg/serverName": {"www.example.org"}, "cfg/port": {"8080"}, "port/http": {"8081"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status %d:\n%s", resp.StatusCode, alerts(body))
	}
	p := getBlock(t, st, "default/site").GetSpec().GetNetwork().GetPorts()[0]
	if p.GetPort() != 8081 || p.GetTargetPort() != 8080 {
		t.Errorf("port = %d→%d, want 8081→8080", p.GetPort(), p.GetTargetPort())
	}

	_, body = get(t, c, srv.URL+"/blocks/default/site/edit")
	if !strings.Contains(body, `name="port/http" value="8081"`) {
		t.Error("the edit form doesn't show the address port")
	}
}
