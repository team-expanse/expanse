package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/expanse/expanse/internal/secrets"
	"github.com/expanse/expanse/internal/store"
)

// sealed unseals the secret the stored block keeps for field.
func sealed(t *testing.T, st store.Store, full, field string) (name, value string) {
	t.Helper()
	for _, r := range getBlock(t, st, full).GetSpec().GetSecrets() {
		if r.GetPath() == field {
			v, err := (&secrets.Store{St: st, ClusterSecret: testClusterSecret}).Get(context.Background(), "default", r.GetName())
			if err != nil {
				t.Fatal(err)
			}
			return r.GetName(), string(v)
		}
	}
	t.Fatalf("%s keeps no secret for %s", full, field)
	return "", ""
}

func storeHolds(t *testing.T, st store.Store, v string) bool {
	t.Helper()
	all, err := st.List(context.Background(), "/")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range all {
		if strings.Contains(string(e.Value), v) {
			return true
		}
	}
	return false
}

func TestDeployFormOffersToGenerateRequiredSecrets(t *testing.T) {
	srv, pw, st := newTestServerStore(t)
	seedNode(t, st, "n1", "")
	c, _ := loggedInClient(t, srv, pw)
	_, body := get(t, c, srv.URL+"/deploy/db/postgres")
	i := strings.Index(body, `name="cfg/superuserPassword"`)
	if i < 0 {
		t.Fatal("no superuserPassword input")
	}
	input := body[i : i+strings.Index(body[i:], ">")]
	if strings.Contains(input, "required") {
		t.Error("a password that is generated when left empty is marked required")
	}
	if !strings.Contains(body, "Leave empty to generate one") {
		t.Error("the form does not say an empty password is generated")
	}
}

func TestDeployGeneratesRequiredSecretsAndShowsThemOnce(t *testing.T) {
	srv, pw, st := newTestServerStore(t)
	seedNode(t, st, "n1", "")
	c, csrf := loggedInClient(t, srv, pw)
	resp, body := postForm(t, c, srv.URL+"/deploy/db/postgres", csrf, url.Values{
		"name": {"pg"}, "replicas": {"1"}, "cfg/replicationPassword": {"given-repl-pass"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want the page with the generated password:\n%s", resp.StatusCode, alerts(body))
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
	_, super := sealed(t, st, "default/pg", "superuserPassword")
	if len(super) < 16 || !strings.Contains(body, super) {
		t.Fatalf("generated password %q is not shown", super)
	}
	if !strings.Contains(body, `href="/blocks/default/pg"`) {
		t.Error("no way on to the block")
	}
	if _, repl := sealed(t, st, "default/pg", "replicationPassword"); repl != "given-repl-pass" || strings.Contains(body, repl) {
		t.Errorf("the given password %q was replaced or shown", repl)
	}
	for _, v := range []string{super, "given-repl-pass"} {
		if storeHolds(t, st, v) {
			t.Errorf("%q is in the store in plain text", v)
		}
	}
	if _, detail := get(t, c, srv.URL+"/blocks/default/pg"); strings.Contains(detail, super) {
		t.Error("the block page shows the password")
	}
}

func TestDeployWithEverySecretGivenRedirects(t *testing.T) {
	srv, pw, st := newTestServerStore(t)
	seedNode(t, st, "n1", "")
	c, csrf := loggedInClient(t, srv, pw)
	resp, body := postForm(t, c, srv.URL+"/deploy/db/postgres", csrf, url.Values{
		"name": {"pg"}, "replicas": {"1"}, "cfg/replicationPassword": {"r-pass"}, "cfg/superuserPassword": {"s-pass"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status %d:\n%s", resp.StatusCode, alerts(body))
	}
}

func TestEditKeepsOrReplacesAStoredSecret(t *testing.T) {
	srv, pw, st := newTestServerStore(t)
	seedNode(t, st, "n1", "")
	c, csrf := loggedInClient(t, srv, pw)
	if resp, body := postForm(t, c, srv.URL+"/deploy/db/postgres", csrf, url.Values{
		"name": {"pg"}, "replicas": {"1"}, "cfg/replicationPassword": {"r-pass"}, "cfg/superuserPassword": {"s-pass"},
	}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("deploy: %d\n%s", resp.StatusCode, alerts(body))
	}
	first, _ := sealed(t, st, "default/pg", "superuserPassword")

	_, body := get(t, c, srv.URL+"/blocks/default/pg/edit")
	if !strings.Contains(body, "Leave empty to keep it") || strings.Contains(body, "s-pass") {
		t.Error("the edit form does not offer to keep the stored password, or shows it")
	}
	resp, body := postForm(t, c, srv.URL+"/blocks/default/pg/edit", csrf, url.Values{"name": {"pg"}, "replicas": {"1"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("edit with the passwords left empty: %d\n%s", resp.StatusCode, alerts(body))
	}
	if name, v := sealed(t, st, "default/pg", "superuserPassword"); name != first || v != "s-pass" {
		t.Errorf("left empty, the password changed: %s=%q", name, v)
	}

	resp, body = postForm(t, c, srv.URL+"/blocks/default/pg/edit", csrf, url.Values{
		"name": {"pg"}, "replicas": {"1"}, "cfg/superuserPassword": {"new-s-pass"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("edit with a new password: %d\n%s", resp.StatusCode, alerts(body))
	}
	if name, v := sealed(t, st, "default/pg", "superuserPassword"); name == first || v != "new-s-pass" {
		t.Errorf("the new password was not stored: %s=%q", name, v)
	}
}
