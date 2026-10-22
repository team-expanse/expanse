package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/cluster/join"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/upgrade"
	"github.com/expanse/expanse/internal/upgrade/rollout"
)

func newUpgradeTestServer(t *testing.T) (*http.Client, string, string, store.Store) {
	t.Helper()
	srv, pw, st := newTestServerWith(t, func(s *Server) { s.cluster = previewCluster{} })
	c, csrf := loggedInClient(t, srv, pw)
	ctx := context.Background()
	for _, n := range []string{"n1", "n2", "n3"} {
		if err := rollout.SaveReport(ctx, st, n, rollout.Report{Version: "1.11.3", Updated: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := rollout.SaveLatest(ctx, st, rollout.Latest{Version: "1.12.0", Checked: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return c, srv.URL, csrf, st
}

func TestUpgradePageShowsVersionsAndOffersTheNewestRelease(t *testing.T) {
	c, base, _, _ := newUpgradeTestServer(t)
	_, body := get(t, c, base+"/upgrade")
	for _, want := range []string{"1.12.0", "1.11.3", `href="/nodes/n2"`, `<option value="1.12.0" selected>1.12.0 (newest)</option>`, `action="/upgrade/start"`, `action="/upgrade/check"`} {
		if !strings.Contains(body, want) {
			t.Errorf("upgrade page missing %q", want)
		}
	}
}

func TestUpgradeOffersTheReleasesFromTheClustersOnward(t *testing.T) {
	c, base, _, st := newUpgradeTestServer(t)
	l := rollout.Latest{Version: "1.12.0", Releases: []string{"1.12.0", "1.11.4", "1.11.3", "1.11.0"}, Checked: time.Now()}
	if err := rollout.SaveLatest(context.Background(), st, l); err != nil {
		t.Fatal(err)
	}
	_, body := get(t, c, base+"/upgrade")
	for _, want := range []string{`<select class="input" id="upgrade-version-`, `<option value="1.12.0" selected>1.12.0 (newest)</option>`, `<option value="1.11.4">1.11.4</option>`, `<option value="1.11.3">1.11.3 (running)</option>`} {
		if !strings.Contains(body, want) {
			t.Errorf("upgrade page missing %q", want)
		}
	}
	for _, unwanted := range []string{`value="1.11.0"`, `type="text" name="version"`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("upgrade page has %q", unwanted)
		}
	}
}

func TestUpgradeAsksForAVersionBeforeAnyCheck(t *testing.T) {
	c, base, _, st := newUpgradeTestServer(t)
	if err := rollout.SaveLatest(context.Background(), st, rollout.Latest{}); err != nil {
		t.Fatal(err)
	}
	_, body := get(t, c, base+"/upgrade")
	if !strings.Contains(body, `type="text" name="version"`) || strings.Contains(body, "<select") {
		t.Error("without a release list the page does not ask for a version")
	}
}

func TestAnAvailableUpgradeShowsOnEveryPage(t *testing.T) {
	c, base, _, st := newUpgradeTestServer(t)
	ctx := context.Background()
	_, body := get(t, c, base+"/nodes")
	for _, want := range []string{`class="nav-badge"`, `class="nav-dot"`, `href="/upgrade">1.12.0 available`} {
		if !strings.Contains(body, want) {
			t.Errorf("nodes page missing %q", want)
		}
	}
	_, body = get(t, c, base+"/upgrade")
	if strings.Count(body, "Update available") != 3 || !strings.Contains(body, "Expanse 1.12.0 is available") {
		t.Error("the upgrade page does not mark the nodes behind the newest release")
	}

	for _, n := range []string{"n1", "n2", "n3"} {
		if err := rollout.SaveReport(ctx, st, n, rollout.Report{Version: "1.12.0", Updated: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := rollout.SaveReport(ctx, st, "removed", rollout.Report{Version: "1.0.0", Updated: time.Now()}); err != nil {
		t.Fatal(err)
	}
	_, body = get(t, c, base+"/nodes")
	if strings.Contains(body, "nav-badge") || strings.Contains(body, "1.12.0 available") {
		t.Error("an up-to-date cluster shows an upgrade")
	}
}

func TestUpgradeStartWritesAPlanWithTheLeaderLast(t *testing.T) {
	c, base, csrf, st := newUpgradeTestServer(t)
	resp, body := postForm(t, c, base+"/upgrade/start", csrf, url.Values{"version": {"1.12.0"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("start: status %d:\n%s", resp.StatusCode, alerts(body))
	}
	p, err := rollout.LoadPlan(context.Background(), st)
	if err != nil || p == nil {
		t.Fatalf("plan %v, err %v", p, err)
	}
	if p.Target != "1.12.0" || p.State != rollout.Running || strings.Join(p.Order, ",") != "n2,n3,n1" {
		t.Errorf("plan = %+v", p)
	}
	resp, body = postForm(t, c, base+"/upgrade/start", csrf, url.Values{"version": {"1.12.1"}})
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, "already") {
		t.Errorf("a second rollout: status %d", resp.StatusCode)
	}
}

func TestUpgradeStartRefusesWhatIsNotAVersion(t *testing.T) {
	c, base, csrf, st := newUpgradeTestServer(t)
	resp, _ := postForm(t, c, base+"/upgrade/start", csrf, url.Values{"version": {"latest; rm -rf /"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d", resp.StatusCode)
	}
	if p, _ := rollout.LoadPlan(context.Background(), st); p != nil {
		t.Errorf("a plan was stored: %+v", p)
	}
}

func TestUpgradePauseResumeAndCancel(t *testing.T) {
	c, base, csrf, st := newUpgradeTestServer(t)
	ctx := context.Background()
	postForm(t, c, base+"/upgrade/start", csrf, url.Values{"version": {"1.12.0"}})
	started, _ := rollout.LoadPlan(ctx, st)

	postForm(t, c, base+"/upgrade/pause", csrf, nil)
	if p, _ := rollout.LoadPlan(ctx, st); p.State != rollout.Paused {
		t.Errorf("after pause: %+v", p)
	}
	time.Sleep(time.Millisecond)
	postForm(t, c, base+"/upgrade/resume", csrf, nil)
	p, _ := rollout.LoadPlan(ctx, st)
	if p.State != rollout.Running || !p.Started.After(started.Started) {
		t.Errorf("after resume: %+v (a resume must retry a failed node)", p)
	}
	postForm(t, c, base+"/upgrade/cancel", csrf, nil)
	if p, _ := rollout.LoadPlan(ctx, st); p.State != rollout.Cancelled {
		t.Errorf("after cancel: %+v", p)
	}
	if resp, _ := postForm(t, c, base+"/upgrade/start", csrf, url.Values{"version": {"1.12.0"}}); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("a new rollout after a cancel: status %d", resp.StatusCode)
	}
}

func TestUpgradePageShowsWhyARolloutHalted(t *testing.T) {
	c, base, _, st := newUpgradeTestServer(t)
	ctx := context.Background()
	rollout.SavePlan(ctx, st, rollout.Plan{
		Target: "1.12.0", State: rollout.Halted, Order: []string{"n2", "n3", "n1"},
		Current: "n2", Reason: "n2: build: builder failed",
	})
	rollout.SaveReport(ctx, st, "n2", rollout.Report{Version: "1.11.3", Upgrade: &upgrade.Status{
		Version: "1.12.0", State: upgrade.StateFailed, Error: "build: builder failed",
	}})
	_, body := get(t, c, base+"/upgrade")
	for _, want := range []string{"builder failed", `action="/upgrade/resume"`, `action="/upgrade/cancel"`} {
		if !strings.Contains(body, want) {
			t.Errorf("halted page missing %q", want)
		}
	}
	if strings.Contains(body, `action="/upgrade/start"`) {
		t.Error("offers a new rollout while one is halted")
	}
	if !strings.Contains(body, "Disabled while the rollout to 1.12.0 runs") || !strings.Contains(body, `<fieldset disabled aria-describedby="upgrade-busy">`) {
		t.Error("the start form is not shown disabled with its reason")
	}
}

func TestRolloutStepsFollowThePlanOrder(t *testing.T) {
	plan := &rollout.Plan{Target: "1.12.0", State: rollout.Running, Order: []string{"n2", "n3", "n1"}, Current: "n3"}
	reports := map[string]rollout.Report{"n2": {Version: "1.12.0"}, "n3": {Version: "1.11.3"}, "n1": {Version: "1.11.3"}}
	got := rolloutSteps(plan, reports)
	want := []string{"n2 Done", "n3 Upgrading", "n1 Waiting"}
	for i, st := range got {
		if i >= len(want) || st.ID+" "+st.Status.Label != want[i] {
			t.Fatalf("rolloutSteps = %+v, want %v", got, want)
		}
	}
	plan.State = rollout.Halted
	if got := rolloutSteps(plan, reports); got[1].Status.Label != "Halted" || got[1].Status.Tone != "crit" {
		t.Errorf("a halted node shows %+v", got[1].Status)
	}
}

func TestUpgradeCheckNowAndTurningTheCheckOff(t *testing.T) {
	c, base, csrf, st := newUpgradeTestServer(t)
	ctx := context.Background()
	postForm(t, c, base+"/upgrade/check", csrf, nil)
	if l, _ := rollout.LoadLatest(ctx, st); !l.Requested {
		t.Errorf("check now not recorded: %+v", l)
	}
	postForm(t, c, base+"/upgrade/auto-check", csrf, url.Values{"enabled": {"off"}})
	if l, _ := rollout.LoadLatest(ctx, st); !l.Disabled {
		t.Errorf("check not turned off: %+v", l)
	}
	postForm(t, c, base+"/upgrade/auto-check", csrf, url.Values{"enabled": {"on"}})
	if l, _ := rollout.LoadLatest(ctx, st); l.Disabled {
		t.Errorf("check not turned on: %+v", l)
	}
}

// The live stream re-renders the form; hx-preserve keeps the choice until a new check lists releases.
func TestUpgradeVersionChoiceSurvivesLiveUpdates(t *testing.T) {
	c, base, _, st := newUpgradeTestServer(t)
	checked := time.Unix(1700000000, 0)
	if err := rollout.SaveLatest(context.Background(), st, rollout.Latest{Version: "1.12.0", Checked: checked}); err != nil {
		t.Fatal(err)
	}
	_, body := get(t, c, base+"/upgrade")
	if want := `id="upgrade-version-1700000000" hx-preserve`; !strings.Contains(body, want) {
		t.Errorf("version choice missing %q", want)
	}
}

// Two failed reads both reach the page, rather than the second hiding the first.
func TestUpgradePageShowsEveryLoadError(t *testing.T) {
	c, base, _, st := newUpgradeTestServer(t)
	for _, k := range []string{rollout.LatestKey, rollout.PlanKey} {
		if _, err := st.Put(context.Background(), store.Key(k), []byte("{not json")); err != nil {
			t.Fatal(err)
		}
	}
	_, body := get(t, c, base+"/upgrade")
	if !strings.Contains(body, "The release check record is unreadable") || !strings.Contains(body, "corrupt record /upgrade/plan") {
		t.Errorf("upgrade page does not show both errors")
	}
}

// Upgrade forms post through htmx, so a refusal is a toast and the typed version stays.
func TestUpgradeFormsKeepThePageOnARefusal(t *testing.T) {
	c, base, csrf, _ := newUpgradeTestServer(t)
	_, body := get(t, c, base+"/upgrade")
	for _, act := range []string{"/upgrade/start", "/upgrade/check", "/upgrade/auto-check"} {
		if !strings.Contains(body, `hx-post="`+act+`" hx-swap="none"`) {
			t.Errorf("form %s does not post through htmx", act)
		}
	}
	// Without JavaScript a refusal is still a styled page, not bare text.
	resp, body := postForm(t, c, base+"/upgrade/start", csrf, url.Values{"version": {"1.12"}})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, `class="error-page"`) || !strings.Contains(body, "not a release version") {
		t.Errorf("refusal: status %d, styled %v", resp.StatusCode, strings.Contains(body, "error-page"))
	}
}

// The node rows come from the cluster report, so a join reaches the page too.
func TestUpgradeStreamShowsAJoinedNode(t *testing.T) {
	srv, pw, st := newClusterTestServer(t)
	c, _ := loggedInClient(t, srv, pw)
	sse := openSSE(t, c, srv.URL+"/upgrade/events")
	sse.expect("event: upgrade", 5*time.Second)
	putTestNode(t, st, join.NodeRecord{ID: "joiner", RaftAddr: "127.0.0.1:1", APIAddr: "127.0.0.1:2", Role: "voter"}, "")
	sse.expect("joiner", 5*time.Second)
}
