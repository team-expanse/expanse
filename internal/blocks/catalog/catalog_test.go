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

// TestLoadShippedBlocksBatch1 covers the T19 real block types: nginx,
// static-site, node-exporter (plus util/echo = 4 loadable real types).
// Each schema validates a realistic config and rejects a bad field with
// a path-qualified message — the V19 regression against real types, not
// the fixture.
func TestLoadShippedBlocksBatch1(t *testing.T) {
	c, err := Load("../../../nix/blocks")
	if err != nil {
		t.Fatalf("Load(nix/blocks): %v", err)
	}
	for _, id := range []string{"util/echo", "web/nginx", "web/static-site", "monitor/node-exporter"} {
		if !c.HasType(id) {
			t.Errorf("shipped catalog missing %s; got %v", id, c.Types())
		}
	}

	t.Run("nginx", func(t *testing.T) {
		good := mustStruct(t, map[string]any{
			"serverName": "web.example.internal",
			"root":       "/var/www",
			"port":       8080,
			"tls":        map[string]any{"cert": "/run/secrets/tls/cert.pem", "key": "/run/secrets/tls/key.pem"},
		})
		if errs := c.ValidateConfig("web/nginx", good); len(errs) != 0 {
			t.Fatalf("realistic nginx config rejected: %v", errs)
		}
		// Unknown field rejected with its path.
		bad := mustStruct(t, map[string]any{"serverName": "x", "worker_processes": 4})
		if errs := c.ValidateConfig("web/nginx", bad); len(errs) != 1 || !strings.Contains(errs[0], "/worker_processes") {
			t.Errorf("unknown nginx field not rejected with path: %v", errs)
		}
		// TLS present but missing the key path is rejected at /tls.
		badTLS := mustStruct(t, map[string]any{
			"serverName": "x",
			"tls":        map[string]any{"cert": "/cert.pem"},
		})
		if errs := c.ValidateConfig("web/nginx", badTLS); len(errs) != 1 || !strings.Contains(errs[0], "/tls") {
			t.Errorf("incomplete tls not rejected with path: %v", errs)
		}
		d := c.Defaults("web/nginx")
		if d["serverName"] != "web.example.internal" || d["port"] != int(8080) {
			t.Errorf("unexpected nginx defaults: %v", d)
		}
	})

	t.Run("static-site", func(t *testing.T) {
		good := mustStruct(t, map[string]any{
			"index": "<html><body>hi</body></html>",
			"port":  8081,
		})
		if errs := c.ValidateConfig("web/static-site", good); len(errs) != 0 {
			t.Fatalf("realistic static-site config rejected: %v", errs)
		}
		if errs := c.ValidateConfig("web/static-site", mustStruct(t, map[string]any{"port": "80"})); len(errs) != 1 || !strings.Contains(errs[0], "/port") {
			t.Errorf("string port not rejected with path: %v", errs)
		}
	})

	t.Run("node-exporter", func(t *testing.T) {
		good := mustStruct(t, map[string]any{"port": 9100, "webTelemetryPath": "/metrics"})
		if errs := c.ValidateConfig("monitor/node-exporter", good); len(errs) != 0 {
			t.Fatalf("realistic node-exporter config rejected: %v", errs)
		}
		if errs := c.ValidateConfig("monitor/node-exporter", mustStruct(t, map[string]any{"extraFlags": []any{"--foo"}})); len(errs) != 1 || !strings.Contains(errs[0], "/extraFlags") {
			t.Errorf("unknown node-exporter field not rejected with path: %v", errs)
		}
		d := c.Defaults("monitor/node-exporter")
		if d["port"] != int(9100) || d["webTelemetryPath"] != "/metrics" {
			t.Errorf("unexpected node-exporter defaults: %v", d)
		}
	})

	// V19 admission through a real shipped type (nginx), not the fixture.
	b := &pb.Block{
		Metadata: &pb.Metadata{Name: "web-1", Namespace: "default"},
		Spec: &pb.BlockSpec{
			Type: "web/nginx",
			Config: mustStruct(t, map[string]any{
				"serverName": "ok", "oops": 1,
			}),
		},
	}
	ctx := validate.Context{Catalog: c}
	var v19 string
	for _, e := range validate.Validate(b, ctx) {
		if e.Rule == "V19" && strings.Contains(e.Message, "/oops") {
			v19 = e.Message
		}
	}
	if v19 == "" {
		t.Errorf("V19 did not flag unknown nginx config field with path: %v", validate.Validate(b, ctx))
	}
}

// TestLoadShippedBlocksBatch2 covers the T20 real block types: db/redis
// (primary-replica, stateful) and ai/ollama (GPU/devices, large storage).
// Includes the V7 and V23 admission regressions against these real types
// with a fake device inventory.
func TestLoadShippedBlocksBatch2(t *testing.T) {
	c, err := Load("../../../nix/blocks")
	if err != nil {
		t.Fatalf("Load(nix/blocks): %v", err)
	}
	for _, id := range []string{"db/redis", "ai/ollama"} {
		if !c.HasType(id) {
			t.Errorf("shipped catalog missing %s; got %v", id, c.Types())
		}
	}

	t.Run("redis", func(t *testing.T) {
		good := mustStruct(t, map[string]any{
			"port":            6379,
			"maxMemory":       "256mb",
			"maxMemoryPolicy": "allkeys-lru",
			"appendOnly":      true,
			"replicaOf":       "redis-0.default.svc:6379",
		})
		if errs := c.ValidateConfig("db/redis", good); len(errs) != 0 {
			t.Fatalf("realistic redis config rejected: %v", errs)
		}
		if errs := c.ValidateConfig("db/redis", mustStruct(t, map[string]any{"maxMemoryPolicy": "nuke-everything"})); len(errs) != 1 || !strings.Contains(errs[0], "/maxMemoryPolicy") {
			t.Errorf("bad enum not rejected with path: %v", errs)
		}
		d := c.Defaults("db/redis")
		if d["port"] != int(6379) || d["maxMemory"] != "256mb" {
			t.Errorf("unexpected redis defaults: %v", d)
		}
	})

	t.Run("ollama", func(t *testing.T) {
		good := mustStruct(t, map[string]any{
			"port":      11434,
			"models":    []any{"llama3:8b"},
			"keepAlive": "5m",
		})
		if errs := c.ValidateConfig("ai/ollama", good); len(errs) != 0 {
			t.Fatalf("realistic ollama config rejected: %v", errs)
		}
		if errs := c.ValidateConfig("ai/ollama", mustStruct(t, map[string]any{"gpuLayers": 32})); len(errs) != 1 || !strings.Contains(errs[0], "/gpuLayers") {
			t.Errorf("unknown ollama field not rejected with path: %v", errs)
		}
	})

	// V7 regression: primary-replica through the real db/redis type with
	// replicas=1 is rejected.
	one := int32(1)
	b := &pb.Block{
		Metadata: &pb.Metadata{Name: "redis-1", Namespace: "default"},
		Spec: &pb.BlockSpec{
			Type:     "db/redis",
			Strategy: &pb.Strategy{Kind: pb.StrategyKind_PRIMARY_REPLICA},
			Replicas: &one,
		},
	}
	ctx := validate.Context{Catalog: c}
	if errs := validate.Validate(b, ctx); len(errs) != 1 || errs[0].Rule != "V7" {
		t.Errorf("primary-replica replicas=1 not rejected as V7: %v", errs)
	}
	// replicas >= 2 passes V7.
	two := int32(2)
	b.Spec.Replicas = &two
	for _, e := range validate.Validate(b, ctx) {
		if e.Rule == "V7" {
			t.Errorf("replicas=2 wrongly rejected: %v", e)
		}
	}

	// V23 regression: ai/ollama requesting a gpu the cluster doesn't
	// have (empty fake device inventory) is rejected, naming what IS
	// available; with a gpu in the inventory it passes.
	b = &pb.Block{
		Metadata: &pb.Metadata{Name: "ollama-1", Namespace: "default"},
		Spec: &pb.BlockSpec{
			Type: "ai/ollama",
			Resources: &pb.Resources{Devices: []*pb.Device{
				{Type: "gpu", Count: 1, Vram: "8Gi"},
			}},
		},
	}
	ctx = validate.Context{Catalog: c, Devices: map[string]int32{}}
	errs := validate.Validate(b, ctx)
	found := false
	for _, e := range errs {
		if e.Rule == "V23" && strings.Contains(e.Message, "gpu") {
			found = true
		}
	}
	if !found {
		t.Errorf("gpu request against empty inventory not rejected as V23: %v", errs)
	}
	// Fake device inventory with gpus available — V23 passes.
	ctx = validate.Context{Catalog: c, Devices: map[string]int32{"gpu": 4}}
	for _, e := range validate.Validate(b, ctx) {
		if e.Rule == "V23" {
			t.Errorf("gpu request wrongly rejected with inventory: %v", e)
		}
	}
}

// TestLoadShippedBlocksBatch3 covers db/postgres (PHASE-05-TASKS.md Stream
// A): a plain active-active, stateful block whose two secret fields are
// required, not defaulted.
func TestLoadShippedBlocksBatch3(t *testing.T) {
	c, err := Load("../../../nix/blocks")
	if err != nil {
		t.Fatalf("Load(nix/blocks): %v", err)
	}
	if !c.HasType("db/postgres") {
		t.Errorf("shipped catalog missing db/postgres; got %v", c.Types())
	}

	t.Run("postgres", func(t *testing.T) {
		good := mustStruct(t, map[string]any{
			"port":                5432,
			"database":            "app",
			"replicationPassword": "s3cret",
			"superuserPassword":   "als0-s3cret",
		})
		if errs := c.ValidateConfig("db/postgres", good); len(errs) != 0 {
			t.Fatalf("realistic postgres config rejected: %v", errs)
		}
		if errs := c.ValidateConfig("db/postgres", mustStruct(t, map[string]any{"port": 5432})); len(errs) == 0 {
			t.Errorf("missing required replicationPassword/superuserPassword not rejected")
		}
		d := c.Defaults("db/postgres")
		if d["port"] != int(5432) || d["database"] != "app" {
			t.Errorf("unexpected postgres defaults: %v", d)
		}
		if _, ok := d["replicationPassword"]; ok {
			t.Errorf("replicationPassword must not have a default (it's a secret): %v", d)
		}
	})
}

// vm/instance accepts guestReady systemd or none, and rejects anything else at apply time.
func TestVMInstanceGuestReadyConfig(t *testing.T) {
	c, err := Load("../../../nix/blocks")
	if err != nil {
		t.Fatalf("Load(nix/blocks): %v", err)
	}
	for _, v := range []string{"systemd", "none"} {
		cfg, _ := structpb.NewStruct(map[string]any{"guestReady": v})
		if errs := c.ValidateConfig("vm/instance", cfg); len(errs) != 0 {
			t.Errorf("guestReady %q rejected: %v", v, errs)
		}
	}
	cfg, _ := structpb.NewStruct(map[string]any{"guestReady": "sometimes"})
	if errs := c.ValidateConfig("vm/instance", cfg); len(errs) != 1 || !strings.HasPrefix(errs[0], "/guestReady:") {
		t.Errorf("guestReady \"sometimes\" errors = %v, want one naming /guestReady", errs)
	}
}

func TestShippedMonitoringBlocksValidate(t *testing.T) {
	c, err := Load("../../../nix/blocks")
	if err != nil {
		t.Fatalf("Load(nix/blocks): %v", err)
	}
	cases := []struct {
		typ     string
		cfg     map[string]any
		wantErr string // "" = valid
	}{
		{"monitor/prometheus", nil, ""},
		{"monitor/prometheus", map[string]any{"metricsToken": "t", "scrapeInterval": "2s", "retention": "30d"}, ""},
		{"monitor/prometheus", map[string]any{"scrapeInterval": "five"}, "/scrapeInterval"},
		{"monitor/grafana", map[string]any{"prometheusUrl": "http://10.0.0.50:9090", "adminPassword": "correct-horse"}, ""},
		{"monitor/grafana", map[string]any{"adminPassword": "correct-horse"}, "prometheusUrl"},
		{"monitor/grafana", map[string]any{"prometheusUrl": "10.0.0.50:9090", "adminPassword": "correct-horse"}, "/prometheusUrl"},
		{"monitor/grafana", map[string]any{"prometheusUrl": "http://p:9090", "adminPassword": "short"}, "/adminPassword"},
	}
	for _, tc := range cases {
		var cfg *structpb.Struct
		if tc.cfg != nil {
			cfg = mustStruct(t, tc.cfg)
		}
		errs := c.ValidateConfig(tc.typ, cfg)
		switch {
		case tc.wantErr == "" && len(errs) != 0:
			t.Errorf("%s %v rejected: %v", tc.typ, tc.cfg, errs)
		case tc.wantErr != "" && (len(errs) == 0 || !strings.Contains(strings.Join(errs, ";"), tc.wantErr)):
			t.Errorf("%s %v = %v, want an error naming %s", tc.typ, tc.cfg, errs, tc.wantErr)
		}
	}
}

func TestShippedNFSBlockValidates(t *testing.T) {
	c, err := Load("../../../nix/blocks")
	if err != nil {
		t.Fatalf("Load(nix/blocks): %v", err)
	}
	cases := []struct {
		cfg     map[string]any
		wantErr string // "" = valid
	}{
		{nil, ""},
		{map[string]any{"port": 12049, "pseudo": "/media", "path": "pub", "squash": "none", "gracePeriod": 30}, ""},
		{map[string]any{"squash": "some"}, "/squash"},
		{map[string]any{"pseudo": "media"}, "/pseudo"},
		{map[string]any{"path": "/etc"}, "/path"},
		{map[string]any{"gracePeriod": 1}, "/gracePeriod"},
	}
	for _, tc := range cases {
		var cfg *structpb.Struct
		if tc.cfg != nil {
			cfg = mustStruct(t, tc.cfg)
		}
		errs := c.ValidateConfig("share/nfs", cfg)
		switch {
		case tc.wantErr == "" && len(errs) != 0:
			t.Errorf("%v rejected: %v", tc.cfg, errs)
		case tc.wantErr != "" && (len(errs) == 0 || !strings.Contains(strings.Join(errs, ";"), tc.wantErr)):
			t.Errorf("%v = %v, want an error naming %s", tc.cfg, errs, tc.wantErr)
		}
	}
}

func TestShippedS3BlockValidates(t *testing.T) {
	c, err := Load("../../../nix/blocks")
	if err != nil {
		t.Fatalf("Load(nix/blocks): %v", err)
	}
	key := map[string]any{
		"accessKeyId":     "GK0123456789abcdef01234567",
		"secretAccessKey": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
	with := func(k string, v any) map[string]any {
		m := map[string]any{k: v}
		for k2, v2 := range key {
			if k2 != k {
				m[k2] = v2
			}
		}
		return m
	}
	cases := []struct {
		cfg     map[string]any
		wantErr string // "" = valid
	}{
		{key, ""},
		{with("buckets", []any{"media", "logs.2026"}), ""},
		{nil, "accessKeyId"},
		{with("accessKeyId", "AKIAEXAMPLE"), "/accessKeyId"},
		{with("secretAccessKey", "short"), "/secretAccessKey"},
		{with("buckets", []any{"Bad_Name"}), "/buckets"},
		{with("region", "EU West"), "/region"},
	}
	for _, tc := range cases {
		var cfg *structpb.Struct
		if tc.cfg != nil {
			cfg = mustStruct(t, tc.cfg)
		}
		errs := c.ValidateConfig("storage/s3", cfg)
		switch {
		case tc.wantErr == "" && len(errs) != 0:
			t.Errorf("%v rejected: %v", tc.cfg, errs)
		case tc.wantErr != "" && (len(errs) == 0 || !strings.Contains(strings.Join(errs, ";"), tc.wantErr)):
			t.Errorf("%v = %v, want an error naming %s", tc.cfg, errs, tc.wantErr)
		}
	}
}

func TestShippedMariaDBBlockValidates(t *testing.T) {
	c, err := Load("../../../nix/blocks")
	if err != nil {
		t.Fatalf("Load(nix/blocks): %v", err)
	}
	with := func(kv ...any) map[string]any {
		m := map[string]any{"rootPassword": "rootsecret"}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	cases := []struct {
		cfg     map[string]any
		wantErr string // "" = valid
	}{
		{with(), ""},
		{with("database", "app", "user", "app", "password", "apppass", "port", 13306, "bufferPool", "1G"), ""},
		{nil, "rootPassword"},
		{with("rootPassword", "short"), "/rootPassword"},
		{with("database", "a-b"), "/database"},
		{with("user", "o'brien"), "/user"},
		{with("bufferPool", "lots"), "/bufferPool"},
		{with("maxConnections", 0), "/maxConnections"},
		{with("user", "app", "database", "app"), "password"},
		{with("user", "app", "password", "apppass"), "database"},
		{with("password", "apppass"), "user"},
	}
	for _, tc := range cases {
		var cfg *structpb.Struct
		if tc.cfg != nil {
			cfg = mustStruct(t, tc.cfg)
		}
		errs := c.ValidateConfig("db/mariadb", cfg)
		switch {
		case tc.wantErr == "" && len(errs) != 0:
			t.Errorf("%v rejected: %v", tc.cfg, errs)
		case tc.wantErr != "" && (len(errs) == 0 || !strings.Contains(strings.Join(errs, ";"), tc.wantErr)):
			t.Errorf("%v = %v, want an error naming %s", tc.cfg, errs, tc.wantErr)
		}
	}
}

// validateCases checks each config against typ's schema; wantErr "" means valid.
func validateCases(t *testing.T, typ string, cases []struct {
	cfg     map[string]any
	wantErr string
},
) {
	t.Helper()
	c, err := Load("../../../nix/blocks")
	if err != nil {
		t.Fatalf("Load(nix/blocks): %v", err)
	}
	for _, tc := range cases {
		var cfg *structpb.Struct
		if tc.cfg != nil {
			cfg = mustStruct(t, tc.cfg)
		}
		errs := c.ValidateConfig(typ, cfg)
		switch {
		case tc.wantErr == "" && len(errs) != 0:
			t.Errorf("%v rejected: %v", tc.cfg, errs)
		case tc.wantErr != "" && (len(errs) == 0 || !strings.Contains(strings.Join(errs, ";"), tc.wantErr)):
			t.Errorf("%v = %v, want an error naming %s", tc.cfg, errs, tc.wantErr)
		}
	}
}

func TestShippedCaddyBlockValidates(t *testing.T) {
	site := func(kv ...any) map[string]any {
		m := map[string]any{"host": "app.example.com", "reverseProxy": []any{"10.0.0.5:8080"}}
		for i := 0; i+1 < len(kv); i += 2 {
			if kv[i+1] == nil {
				delete(m, kv[i].(string))
			} else {
				m[kv[i].(string)] = kv[i+1]
			}
		}
		return m
	}
	sites := func(s ...map[string]any) map[string]any {
		l := []any{}
		for _, x := range s {
			l = append(l, x)
		}
		return map[string]any{"sites": l}
	}
	validateCases(t, "web/caddy", []struct {
		cfg     map[string]any
		wantErr string
	}{
		{sites(site()), ""},
		{sites(site("tls", "internal"), site("host", "plain.example.com", "tls", "off", "reverseProxy", nil, "respond", "ok")), ""},
		{map[string]any{"caddyfile": ":18080 {\n\trespond ok\n}\n"}, ""},
		{map[string]any{"sites": []any{site()}, "email": "ops@example.com", "port": 18080, "httpsPort": 18443}, ""},
		{nil, "sites"},
		{map[string]any{"sites": []any{}}, "/sites"},
		{sites(site("host", "bad host")), "/sites/0/host"},
		{sites(site("tls", "maybe")), "/sites/0/tls"},
		{sites(site("reverseProxy", nil)), "/sites/0"},
		{sites(site("respond", "ok")), "/sites/0/reverseProxy"},
		{sites(site("reverseProxy", []any{"no port"})), "/sites/0/reverseProxy/0"},
		{sites(site("respond", "two\nlines", "reverseProxy", nil)), "/sites/0/respond"},
		{map[string]any{"sites": []any{site()}, "caddyfile": "x"}, "/sites"},
		{map[string]any{"sites": []any{site()}, "email": "not-an-email"}, "/email"},
	})
}

func TestShippedHAProxyBlockValidates(t *testing.T) {
	fe := func(kv ...any) map[string]any {
		m := map[string]any{"name": "web", "port": 18080, "servers": []any{"10.0.0.5:8080", "app2.lan:8080"}}
		for i := 0; i+1 < len(kv); i += 2 {
			if kv[i+1] == nil {
				delete(m, kv[i].(string))
			} else {
				m[kv[i].(string)] = kv[i+1]
			}
		}
		return m
	}
	fes := func(f ...map[string]any) map[string]any {
		l := []any{}
		for _, x := range f {
			l = append(l, x)
		}
		return map[string]any{"frontends": l}
	}
	validateCases(t, "net/haproxy", []struct {
		cfg     map[string]any
		wantErr string
	}{
		{fes(fe()), ""},
		{fes(fe("mode", "tcp", "balance", "leastconn", "check", false), fe("name", "api", "port", 18081, "checkPath", "/healthz")), ""},
		{map[string]any{"config": "global\n\tmaxconn 100\n"}, ""},
		{map[string]any{"frontends": []any{fe()}, "statsPort": 18404, "maxconn": 4096}, ""},
		{nil, "frontends"},
		{map[string]any{"frontends": []any{}}, "/frontends"},
		{fes(fe("name", "bad name")), "/frontends/0/name"},
		{fes(fe("mode", "udp")), "/frontends/0/mode"},
		{fes(fe("balance", "random-ish")), "/frontends/0/balance"},
		{fes(fe("servers", []any{})), "/frontends/0/servers"},
		{fes(fe("servers", []any{"10.0.0.5"})), "/frontends/0/servers/0"},
		{fes(fe("port", nil)), "port"},
		{fes(fe("checkPath", "no-slash")), "/frontends/0/checkPath"},
		{map[string]any{"frontends": []any{fe()}, "config": "x"}, "/frontends"},
	})
}

func TestShippedForgejoBlockValidates(t *testing.T) {
	cfg := func(kv ...any) map[string]any {
		m := map[string]any{
			"rootURL": "http://git.example.com/", "adminUser": "gitadmin",
			"adminPassword": "s3cret-pass", "adminEmail": "ops@example.com",
		}
		for i := 0; i+1 < len(kv); i += 2 {
			if kv[i+1] == nil {
				delete(m, kv[i].(string))
			} else {
				m[kv[i].(string)] = kv[i+1]
			}
		}
		return m
	}
	validateCases(t, "dev/forgejo", []struct {
		cfg     map[string]any
		wantErr string
	}{
		{cfg(), ""},
		{cfg("rootURL", "https://git.example.com:8443/forge", "publicSSHPort", 22, "allowRegistration", true), ""},
		{cfg("settings", map[string]any{"database": map[string]any{"DB_TYPE": "mysql", "MAX_OPEN_CONNS": 10}}), ""},
		{nil, "rootURL"},
		{cfg("rootURL", "git.example.com"), "/rootURL"},
		{cfg("adminPassword", "short"), "/adminPassword"},
		{cfg("adminUser", "a b"), "/adminUser"},
		{cfg("sshPort", 22), "/sshPort"},
		{cfg("settings", map[string]any{"bad section": map[string]any{"K": "v"}}), "/settings"},
		{cfg("settings", map[string]any{"server": map[string]any{"K": "a\nb"}}), "/settings/server/K"},
		{cfg("settings", map[string]any{"server": "v"}), "/settings/server"},
	})
}

func TestShippedResticBackupBlockValidates(t *testing.T) {
	cfg := func(kv ...any) map[string]any {
		m := map[string]any{
			"repository": "s3:http://10.0.0.9:3900/backups", "password": "repo-pass",
			"volumes": []any{"forge/forgejo-data"},
		}
		for i := 0; i+1 < len(kv); i += 2 {
			if kv[i+1] == nil {
				delete(m, kv[i].(string))
			} else {
				m[kv[i].(string)] = kv[i+1]
			}
		}
		return m
	}
	validateCases(t, "util/restic-backup", []struct {
		cfg     map[string]any
		wantErr string
	}{
		{cfg(), ""},
		{cfg("interval", "6h", "keep", map[string]any{"last": 3, "daily": 7}), ""},
		{cfg("env", map[string]any{"AWS_ACCESS_KEY_ID": "GK1", "AWS_SECRET_ACCESS_KEY": "s"}), ""},
		{cfg("volumes", []any{"forge/forgejo-data", "scratch"}), ""},
		{nil, "repository"},
		{cfg("password", nil), "password"},
		{cfg("volumes", []any{}), "/volumes"},
		{cfg("volumes", []any{"a/b/c"}), "/volumes/0"},
		{cfg("volumes", []any{"x", "x"}), "/volumes"},
		{cfg("interval", "daily"), "/interval"},
		{cfg("keep", map[string]any{"daily": 0}), "/keep/daily"},
		{cfg("keep", map[string]any{"forever": 1}), "/keep"},
		{cfg("env", map[string]any{"RESTIC_PASSWORD": "v"}), "/env"},
		{cfg("env", map[string]any{"bad name": "v"}), "/env"},
	})
}

func TestShippedVaultwardenBlockValidates(t *testing.T) {
	cfg := func(kv ...any) map[string]any {
		m := map[string]any{"domain": "https://vault.example.com"}
		for i := 0; i+1 < len(kv); i += 2 {
			if kv[i+1] == nil {
				delete(m, kv[i].(string))
			} else {
				m[kv[i].(string)] = kv[i+1]
			}
		}
		return m
	}
	validateCases(t, "security/vaultwarden", []struct {
		cfg     map[string]any
		wantErr string
	}{
		{cfg(), ""},
		{cfg("domain", "http://10.0.0.5:8080/vault", "signupsAllowed", true, "adminToken", "$argon2id$v=19$m=65540,t=3,p=4$abc$def"), ""},
		{cfg("settings", map[string]any{"SMTP_HOST": "mail.example.com", "SMTP_PORT": 587, "SMTP_SSL": true}), ""},
		{nil, "domain"},
		{cfg("domain", "vault.example.com"), "/domain"},
		{cfg("adminToken", ""), "/adminToken"},
		{cfg("port", 80), "/port"},
		{cfg("settings", map[string]any{"smtp_host": "x"}), "/settings"},
		{cfg("settings", map[string]any{"SMTP_HOST": "a\nb"}), "/settings/SMTP_HOST"},
		{cfg("settings", map[string]any{"SMTP_HOST": map[string]any{}}), "/settings/SMTP_HOST"},
		{cfg("bogus", 1), "bogus"},
	})
}
