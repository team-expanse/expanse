package web

import (
	"io"
	"net/http"
	"strings"
	"testing"

	pb "github.com/expanse/expanse/proto"
)

func TestBlockDetailOffersSuspendOrResumeForSuspendableTypes(t *testing.T) {
	tmpl, err := loadTemplates(templateFS, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		suspendable bool
		power       pb.Power
		want        string
	}{
		{false, pb.Power_POWER_ON, ""},
		{true, pb.Power_POWER_ON, ">Suspend<"},
		{true, pb.Power_POWER_UNSPECIFIED, ">Suspend<"},
		{true, pb.Power_POWER_SUSPENDED, ">Resume<"},
	} {
		b := &pb.Block{Metadata: &pb.Metadata{Namespace: "default", Name: "vm"}, Spec: &pb.BlockSpec{Type: "vm/instance", Power: c.power}}
		var buf strings.Builder
		if err := tmpl.execute(&buf, "block_detail.html", blockDetailData{Block: b, Suspendable: c.suspendable}); err != nil {
			t.Fatal(err)
		}
		body := buf.String()
		if card := strings.Contains(body, "<h2>Power</h2>"); card != c.suspendable {
			t.Errorf("%+v: Power card shown = %v", c, card)
		}
		if c.want != "" && !strings.Contains(body, c.want) {
			t.Errorf("%+v: no %s button", c, c.want)
		}
	}
}

func TestBlockPowerRejectsBadRequests(t *testing.T) {
	srv, pw := newTestServer(t)
	client, csrf := loggedInClient(t, srv, pw)
	post := func(path, form string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set(csrfHeader, csrf)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if code, _ := post("/blocks", deployForm(echoManifest)); code != http.StatusSeeOther {
		t.Fatalf("deploy: %d", code)
	}
	if code, _ := post("/blocks/default/web/power", "power=off"); code != http.StatusBadRequest {
		t.Errorf("power=off: %d, want 400", code)
	}
	if code, body := post("/blocks/default/web/power", "power=suspended"); code < 400 || !strings.Contains(body, "cannot be suspended") {
		t.Errorf("suspending util/echo: %d %q, want refused", code, body)
	}
}
