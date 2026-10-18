package systemd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// secretManager unseals from vals, keyed "<ns>/<name>".
func secretManager(t *testing.T, vals map[string]string) *Manager {
	t.Helper()
	m := NewManager(&fakeUnitAPI{states: map[string][3]string{}}, &Applier{Cache: newTestCache(t), Builder: &mockBuilder{}})
	m.SpecDir, m.SecretDir = t.TempDir(), t.TempDir()
	m.Unseal = func(_ context.Context, ns, name string) ([]byte, error) {
		v, ok := vals[ns+"/"+name]
		if !ok {
			return nil, errors.New("no such secret")
		}
		return []byte(v), nil
	}
	return m
}

func convergeSpec(t *testing.T, m *Manager, s Spec) error {
	t.Helper()
	ctx := context.Background()
	raw, _ := json.Marshal(s)
	res, err := m.Load("block-replica:default/web/0", raw)
	if err != nil {
		t.Fatal(err)
	}
	o, err := m.Observe(ctx, res)
	if err != nil {
		t.Fatal(err)
	}
	acts, err := m.Plan(ctx, res, o)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range acts {
		if err := m.Apply(ctx, a); err != nil {
			return err
		}
	}
	return nil
}

func TestManagerHandsSecretsToTheUnitAsCredentials(t *testing.T) {
	m := secretManager(t, map[string]string{"default/web.password.aa": "s3cret-a", "default/web.password.bb": "s3cret-b"})
	s := sampleSpec()
	s.Secrets = map[string]string{"password": "web.password.aa"}
	if err := convergeSpec(t, m, s); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(m.SecretDir, Instance(s.Namespace, s.Name, s.Index))
	if got, _ := os.ReadFile(filepath.Join(dir, "password")); string(got) != "s3cret-a" {
		t.Fatalf("credential = %q", got)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("credential dir mode = %v, %v; want 0700", fi.Mode().Perm(), err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "password")); fi.Mode().Perm() != 0o600 {
		t.Errorf("credential mode = %v, want 0600", fi.Mode().Perm())
	}
	spec, _ := os.ReadFile(filepath.Join(m.SpecDir, Instance(s.Namespace, s.Name, s.Index)+".json"))
	if strings.Contains(string(spec), "s3cret") {
		t.Error("the spec file holds the secret")
	}

	s.Secrets = map[string]string{"password": "web.password.bb"}
	if err := convergeSpec(t, m, s); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "password")); string(got) != "s3cret-b" {
		t.Fatalf("after a change, credential = %q", got)
	}

	s.Secrets = nil
	if err := convergeSpec(t, m, s); err != nil {
		t.Fatal(err)
	}
	if ents, err := os.ReadDir(dir); err != nil || len(ents) != 0 {
		t.Errorf("without secrets the dir must exist and be empty (LoadCredential reads it): %v, %v", ents, err)
	}

	res, _ := m.Load("block-replica:default/web/0", mustJSON(t, s))
	if err := m.Delete(context.Background(), res); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("credential dir survived Delete: %v", err)
	}
}

func TestManagerDoesNotStartAUnitWhoseSecretCannotBeRead(t *testing.T) {
	m := secretManager(t, nil)
	s := sampleSpec()
	s.Secrets = map[string]string{"password": "web.password.gone"}
	if err := convergeSpec(t, m, s); err == nil {
		t.Fatal("converged without its secret")
	}
	if st := m.API.(*fakeUnitAPI).states[UnitNameForSpec(s)]; st[1] == "active" {
		t.Error("the unit was started without its secret")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
