package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/expanse/expanse/internal/blocks/validate"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// Compile-time check that the loaded catalog satisfies the T03 admission
// interface (kept in tests so catalog.go does not import validate).
var _ interface {
	HasType(t string) bool
	Types() []string
	ValidateConfig(t string, config *structpb.Struct) []string
} = (*Catalog)(nil)

func mustLoad(t *testing.T) *Catalog {
	t.Helper()
	c, err := Load("testdata/blocks")
	if err != nil {
		t.Fatalf("Load(testdata/blocks): %v", err)
	}
	return c
}

func TestLoadListsTypes(t *testing.T) {
	c := mustLoad(t)
	got := c.Types()
	want := []string{"demo/ping", "demo/pong"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Types() = %v, want %v", got, want)
	}
	if !c.HasType("demo/ping") || !c.HasType("demo/pong") {
		t.Error("HasType false for fixture types")
	}
	if c.HasType("demo/other") {
		t.Error("HasType true for unknown type")
	}
}

func TestLoadMetadata(t *testing.T) {
	c := mustLoad(t)
	typ, ok := c.GetType("demo/ping")
	if !ok {
		t.Fatal("demo/ping not loaded")
	}
	if typ.Version != "1.0.0" || typ.Description != "synthetic ping block for catalog tests" || typ.Category != "demo" || typ.Name != "ping" {
		t.Errorf("unexpected metadata: %+v", typ)
	}
	typ, ok = c.GetType("demo/pong")
	if !ok {
		t.Fatal("demo/pong not loaded")
	}
	if len(typ.Capabilities) != 1 || typ.Capabilities[0] != "net" {
		t.Errorf("capabilities not parsed: %+v", typ.Capabilities)
	}
}

func TestDefaults(t *testing.T) {
	c := mustLoad(t)
	d := c.Defaults("demo/ping")
	if d["target"] != "localhost" || d["count"] != int(3) {
		t.Errorf("defaults not loaded: %v", d)
	}
	// Mutating the returned map must not corrupt the catalog.
	d["target"] = "mutated"
	if c.Defaults("demo/ping")["target"] != "localhost" {
		t.Error("Defaults returned catalog-owned map")
	}
	if c.Defaults("demo/other") != nil {
		t.Error("Defaults non-nil for unknown type")
	}
}

func TestValidateConfigPass(t *testing.T) {
	c := mustLoad(t)
	cfg, _ := structpb.NewStruct(map[string]any{"target": "db.internal", "count": 2})
	if errs := c.ValidateConfig("demo/ping", cfg); len(errs) != 0 {
		t.Fatalf("valid config rejected: %v", errs)
	}
	// nil config validates when the schema has no required fields.
	if errs := c.ValidateConfig("demo/pong", nil); len(errs) != 0 {
		t.Fatalf("nil config rejected: %v", errs)
	}
}

func TestValidateConfigRequiredFieldPath(t *testing.T) {
	c := mustLoad(t)
	cfg, _ := structpb.NewStruct(map[string]any{"count": 2})
	errs := c.ValidateConfig("demo/ping", cfg)
	if len(errs) != 1 {
		t.Fatalf("want 1 error, got %v", errs)
	}
	// V19: the message must name the failing field path.
	if !strings.HasPrefix(errs[0], "/target:") {
		t.Errorf("error %q does not name the field path /target", errs[0])
	}
	if !strings.Contains(errs[0], "missing properties") {
		t.Errorf("error %q does not explain the failure", errs[0])
	}
}

func TestValidateConfigEnumFieldPath(t *testing.T) {
	c := mustLoad(t)
	cfg, _ := structpb.NewStruct(map[string]any{"level": "mid"})
	errs := c.ValidateConfig("demo/pong", cfg)
	if len(errs) != 1 {
		t.Fatalf("want 1 error, got %v", errs)
	}
	// V19: the message must name the failing field path.
	if !strings.HasPrefix(errs[0], "/level:") {
		t.Errorf("error %q does not name the field path /level", errs[0])
	}
}

func TestLoadErrors(t *testing.T) {
	write := func(t *testing.T, dir, rel, content string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("missing schema.json", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "cat/app/block.yaml", "name: app\n")
		if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "schema.json") {
			t.Errorf("missing schema.json not reported: %v", err)
		}
	})

	t.Run("missing block.yaml", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "cat/app/schema.json", "{}")
		if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "block.yaml") {
			t.Errorf("missing block.yaml not reported: %v", err)
		}
	})

	t.Run("name mismatch", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "cat/app/block.yaml", "name: other\n")
		write(t, dir, "cat/app/schema.json", "{}")
		if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "does not match directory") {
			t.Errorf("name mismatch not reported: %v", err)
		}
	})

	t.Run("bad schema", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "cat/app/block.yaml", "name: app\n")
		write(t, dir, "cat/app/schema.json", `{"type": "nonsense"}`)
		if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "compile schema.json") {
			t.Errorf("bad schema not reported: %v", err)
		}
	})

	t.Run("missing dir", func(t *testing.T) {
		if _, err := Load(filepath.Join(t.TempDir(), "nope")); err == nil {
			t.Error("missing dir not reported")
		}
	})
}

// TestValidateIntegration re-runs the T03 admission rules (V3/V19) through
// validate.Validate against the real fixture catalog.
func TestValidateIntegration(t *testing.T) {
	c := mustLoad(t)

	b := &pb.Block{
		Metadata: &pb.Metadata{Name: "web", Namespace: "default"},
		Spec: &pb.BlockSpec{
			Type:     "demo/ping",
			Replicas: func() *int32 { i := int32(1); return &i }(),
			Config:   mustStruct(t, map[string]any{"target": "db.internal"}),
		},
	}
	ctx := ctxFor(t, c)
	if errs := validate.Validate(b, ctx); len(errs) != 0 {
		t.Fatalf("valid block rejected: %v", errs)
	}

	// V3: unknown type names the available types.
	b.Spec.Type = "demo/nope"
	var v3 string
	for _, e := range validate.Validate(b, ctx) {
		if e.Rule == "V3" {
			v3 = e.Message
		}
	}
	if v3 == "" || !strings.Contains(v3, "demo/ping") {
		t.Errorf("V3 message %q does not list fixture types", v3)
	}

	// V19: config violating the real schema names the field path.
	b.Spec.Type = "demo/ping"
	b.Spec.Config, _ = structpb.NewStruct(map[string]any{"count": 2})
	var v19 string
	for _, e := range validate.Validate(b, ctx) {
		if e.Rule == "V19" {
			v19 = e.Message
		}
	}
	if v19 == "" || !strings.Contains(v19, "/target") {
		t.Errorf("V19 message %q does not name the field path", v19)
	}
}

func ctxFor(t *testing.T, c *Catalog) validate.Context {
	t.Helper()
	return validate.Context{Catalog: c}
}

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestLoadRealBlocks loads the shipped block catalog from the repository
// (nix/blocks, not the test fixture) and exercises the util/echo schema.
func TestLoadRealBlocks(t *testing.T) {
	c, err := Load("../../../nix/blocks")
	if err != nil {
		t.Fatalf("Load(nix/blocks): %v", err)
	}
	if !c.HasType("util/echo") {
		t.Fatalf("shipped catalog missing util/echo; got %v", c.Types())
	}

	// A minimal config validates.
	if errs := c.ValidateConfig("util/echo", mustStruct(t, map[string]any{"port": 18080})); len(errs) != 0 {
		t.Fatalf("minimal echo config rejected: %v", errs)
	}
	// nil config validates (no required fields).
	if errs := c.ValidateConfig("util/echo", nil); len(errs) != 0 {
		t.Fatalf("nil echo config rejected: %v", errs)
	}
	// An unknown field is rejected, naming its path.
	errs := c.ValidateConfig("util/echo", mustStruct(t, map[string]any{"bogus": "x"}))
	if len(errs) != 1 || !strings.Contains(errs[0], "/bogus") {
		t.Errorf("unknown field not rejected with path: %v", errs)
	}
	// Out-of-range port rejected with its path.
	errs = c.ValidateConfig("util/echo", mustStruct(t, map[string]any{"port": 70000}))
	if len(errs) != 1 || !strings.Contains(errs[0], "/port") {
		t.Errorf("bad port not rejected with path: %v", errs)
	}
	// Defaults parse.
	d := c.Defaults("util/echo")
	if d["port"] != int(18080) || d["body"] != "expanse echo test workload" {
		t.Errorf("unexpected echo defaults: %v", d)
	}

	// V3/V19 through the admission rules against the shipped type.
	b := &pb.Block{
		Metadata: &pb.Metadata{Name: "echo-1", Namespace: "default"},
		Spec: &pb.BlockSpec{
			Type:     "util/echo",
			Replicas: func() *int32 { i := int32(2); return &i }(),
			Config:   mustStruct(t, map[string]any{"port": 18081, "body": "hi"}),
		},
	}
	ctx := validate.Context{Catalog: c}
	if errs := validate.Validate(b, ctx); len(errs) != 0 {
		t.Fatalf("valid echo block rejected: %v", errs)
	}
	b.Spec.Config = mustStruct(t, map[string]any{"nope": true})
	var v19 string
	for _, e := range validate.Validate(b, ctx) {
		if e.Rule == "V19" {
			v19 = e.Message
		}
	}
	if v19 == "" || !strings.Contains(v19, "/nope") {
		t.Errorf("V19 message %q does not name the unknown field", v19)
	}
}
