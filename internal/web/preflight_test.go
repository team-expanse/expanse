package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/expanse/expanse/internal/blocks/service"
	"github.com/expanse/expanse/internal/quantity"
	"github.com/expanse/expanse/internal/scheduler"
)

// withNodes gives the block service one ready node with 4 cores and 8Gi free.
func withNodes(s *Server) {
	s.blocks.(*service.Server).Nodes = func(context.Context) ([]scheduler.NodeView, scheduler.OvercommitConfig, error) {
		return []scheduler.NodeView{{
			ID: "n1", Ready: true, FreeCPU: quantity.CPU{Milli: 4000},
			FreeMem: quantity.Bytes{N: 8 << 30}, FreeDisk: quantity.Bytes{N: 100 << 30},
		}}, scheduler.OvercommitConfig{CPUOvercommitRatio: 1, MemoryOvercommitRatio: 1}, nil
	}
}

func nginxForm(name, memory, port string) url.Values {
	return url.Values{
		"name": {name}, "replicas": {"1"}, "cpu": {"250m"}, "memory": {memory},
		"cfg/serverName": {"www.example.org"}, "cfg/port": {port},
	}
}

func TestDeployFormShowsWhereTheBlockWouldRun(t *testing.T) {
	srv, pw, st := newTestServerWith(t, withNodes)
	seedNode(t, st, "n1", "")
	c, _ := loggedInClient(t, srv, pw)
	_, body := get(t, c, srv.URL+"/deploy/web/nginx")
	if !strings.Contains(body, "Pre-flight") || !strings.Contains(body, "runs on <b>n1</b>") {
		t.Fatalf("no pre-flight placement on the form:\n%s", body)
	}
}

func TestDeployCheckReportsWhatDoesNotFitAndCreatesNothing(t *testing.T) {
	srv, pw, st := newTestServerWith(t, withNodes)
	seedNode(t, st, "n1", "")
	c, csrf := loggedInClient(t, srv, pw)
	form := nginxForm("site", "64Gi", "8080")
	form.Set("check", "1")
	resp, body := postForm(t, c, srv.URL+"/deploy/web/nginx", csrf, form)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d:\n%s", resp.StatusCode, alerts(body))
	}
	for _, want := range []string{"n1: needs 64Gi memory", "Lower <b>Memory</b>", `value="64Gi"`} {
		if !strings.Contains(body, want) {
			t.Errorf("check page lacks %q", want)
		}
	}
	if _, err := st.Get(context.Background(), "/blocks/default/site"); err == nil {
		t.Error("Check created the block")
	}
}

func TestDeployCheckWarnsOfAPortClash(t *testing.T) {
	srv, pw, st := newTestServerWith(t, withNodes)
	seedNode(t, st, "n1", "")
	c, csrf := loggedInClient(t, srv, pw)
	if resp, body := postForm(t, c, srv.URL+"/deploy/web/nginx", csrf, nginxForm("one", "128Mi", "8080")); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("deploy: %d\n%s", resp.StatusCode, alerts(body))
	}
	form := nginxForm("two", "128Mi", "8080")
	form.Set("check", "1")
	_, body := postForm(t, c, srv.URL+"/deploy/web/nginx", csrf, form)
	if !strings.Contains(body, "Port 8080 is also used by default/one") {
		t.Errorf("no port clash warning:\n%s", body)
	}
}

func TestDeployCheckPassesAFittingBlock(t *testing.T) {
	srv, pw, st := newTestServerWith(t, withNodes)
	seedNode(t, st, "n1", "")
	c, csrf := loggedInClient(t, srv, pw)
	form := nginxForm("site", "128Mi", "8080")
	form.Set("check", "1")
	_, body := postForm(t, c, srv.URL+"/deploy/web/nginx", csrf, form)
	if !strings.Contains(body, "Ready to deploy") {
		t.Errorf("a fitting block is not reported ready:\n%s", body)
	}
}

func TestPreflightSaysANodeIsNotReadyInPlainWords(t *testing.T) {
	for reason, want := range map[string]string{
		"NotReady: node not schedulable (ready=false, cordoned=false)": "n1: not ready to take blocks",
		"NotReady: node not schedulable (ready=true, cordoned=true)":   "n1: cordoned",
		"InsufficientCPU: needs 2 cpu, 1 available":                    "n1: needs 2 cpu, 1 available",
	} {
		if got := problem("n1", reason); got != want {
			t.Errorf("%q: %q, want %q", reason, got, want)
		}
	}
}
