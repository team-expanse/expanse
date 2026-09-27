package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/agent/health"
	"github.com/expanse/expanse/internal/storage"
)

func TestDashboardSummarisesNodesVolumesAndAlerts(t *testing.T) {
	srv, pw, st := newHealthTestServer(t,
		health.Result{Name: "disk-space", Status: health.Healthy},
		health.Result{Name: "memory", Status: health.Unhealthy, Message: "swap exhausted"},
	)
	client, _ := loggedInClient(t, srv, pw)
	putTestVolume(t, st,
		storage.Spec{ID: "vol-1", Name: "data", SizeBytes: 10 << 30, Replication: 3},
		storage.Status{State: storage.StateDegraded, Primary: "n1"},
	)

	resp, body := getPage(t, client, srv, "/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	for _, want := range []string{
		`id="dashboard-fragment"`, "1/1", // nodes up/total
		"Quorum", "ExpanseNodeUnhealthy", "swap exhausted", // firing alert surfaced
		"data", "Degraded", // degraded volume surfaced
		`href="/volumes/data"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	if strings.Contains(body, "The cluster management interface starts here") {
		t.Error("dashboard still renders the placeholder text")
	}
}

func TestDashboardWithoutClusterServiceStillRenders(t *testing.T) {
	srv, pw := newTestServer(t)
	client, _ := loggedInClient(t, srv, pw)
	resp, body := getPage(t, client, srv, "/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, "Standalone") {
		t.Errorf("standalone dashboard should say so: %s", body)
	}
}

func TestDashboardEventsSSEStreamsLiveUpdate(t *testing.T) {
	srv, pw, st := newHealthTestServer(t)
	client, _ := loggedInClient(t, srv, pw)

	stream := openSSE(t, client, srv.URL+"/dashboard/events")
	stream.expect("event: dashboard", 10*time.Second)

	putTestVolume(t, st,
		storage.Spec{ID: "vol-2", Name: "livevol", SizeBytes: 1 << 30, Replication: 1},
		storage.Status{State: storage.StateFailed},
	)
	stream.expect("livevol", 10*time.Second)
}
