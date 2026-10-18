package apply

import (
	"testing"

	"github.com/expanse/expanse/internal/blocks/catalog"
)

func fixtureType(t *testing.T, id string) *catalog.Type {
	t.Helper()
	c, err := catalog.Load("../catalog/testdata/blocks")
	if err != nil {
		t.Fatal(err)
	}
	typ, ok := c.GetType(id)
	if !ok {
		t.Fatalf("fixture type %s missing", id)
	}
	return typ
}

func TestFromTemplateMergesTemplateDefaultsAndConfig(t *testing.T) {
	typ := fixtureType(t, "demo/ping")
	b, err := FromTemplate(typ, "default", "p1", map[string]any{"target": "example.org"})
	if err != nil {
		t.Fatal(err)
	}
	if b.GetMetadata().GetName() != "p1" || b.GetMetadata().GetNamespace() != "default" {
		t.Errorf("metadata = %v", b.GetMetadata())
	}
	s := b.GetSpec()
	if s.GetType() != "demo/ping" || s.GetReplicas() != 2 {
		t.Errorf("type/replicas = %q/%d", s.GetType(), s.GetReplicas())
	}
	if p := s.GetNetwork().GetPorts(); len(p) != 1 || p[0].GetTargetPort() != 8080 {
		t.Errorf("ports = %v", p)
	}
	cfg := s.GetConfig().AsMap()
	if cfg["target"] != "example.org" {
		t.Errorf("config target = %v", cfg["target"])
	}
	for k, v := range typ.Defaults {
		if _, ok := cfg[k]; !ok {
			t.Errorf("default %s=%v missing from config %v", k, v, cfg)
		}
	}
}

func TestFromTemplateConfigOverridesDefaults(t *testing.T) {
	typ := fixtureType(t, "demo/ping")
	for k := range typ.Defaults {
		b, err := FromTemplate(typ, "", "p1", map[string]any{k: "override"})
		if err != nil {
			t.Fatal(err)
		}
		if got := b.GetSpec().GetConfig().AsMap()[k]; got != "override" {
			t.Errorf("config %s = %v, want override", k, got)
		}
		return
	}
	t.Fatal("fixture demo/ping has no defaults")
}

func TestFromTemplateDoesNotMutateType(t *testing.T) {
	typ := fixtureType(t, "demo/ping")
	if _, err := FromTemplate(typ, "", "p1", map[string]any{"extra": 1}); err != nil {
		t.Fatal(err)
	}
	if _, ok := typ.Defaults["extra"]; ok {
		t.Error("FromTemplate wrote into the type's defaults")
	}
	if _, ok := typ.Template["type"]; ok {
		t.Error("FromTemplate wrote into the type's template")
	}
}

func TestFromTemplateWithoutTemplate(t *testing.T) {
	b, err := FromTemplate(fixtureType(t, "demo/pong"), "", "p2", nil)
	if err != nil {
		t.Fatal(err)
	}
	if b.GetSpec().GetType() != "demo/pong" {
		t.Errorf("type = %q", b.GetSpec().GetType())
	}
}
