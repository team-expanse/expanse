package systemd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	nix "github.com/expanse/expanse/internal/agent/nix"
	"github.com/expanse/expanse/internal/reconcile"
	pb "github.com/expanse/expanse/proto"
)

func sampleSpec() Spec {
	return Spec{
		Namespace: "default",
		Name:      "web",
		Index:     0,
		Type:      "util/echo",
		Limits:    &pb.ResourcePair{Cpu: "2", Memory: "1Gi"},
		VolumeMounts: []string{
			"/persist/expanse/blocks/web/z",
			"/persist/expanse/blocks/web/a",
		},
		// T14 §4.7: volume mounts bind into the unit namespace
		// as host:declared-mount-path pairs.
		BindPaths: []string{
			"/var/lib/expanse/volumes/vol-b/mnt:/var/lib/postgresql",
			"/var/lib/expanse/volumes/vol-a/mnt:/var/lib/db",
		},
		Credentials: map[string]string{"db_password": "/persist/expanse/secrets/db_password"},
	}
}

// Unit-property generation from a sample resources block.
func TestUnitFileProperties(t *testing.T) {
	u := UnitFile(sampleSpec())
	for _, want := range []string{
		"Slice=expanse-blocks.slice",
		"SyslogIdentifier=expanse-block-%i",        // §5.5: matches JournaldIdentifier
		"CPUQuota=200%",                            // cpu: "2" (§5.3)
		fmt.Sprintf("MemoryMax=%d", 1<<30),         // 1Gi exact
		fmt.Sprintf("MemoryHigh=%d", (1<<30)*9/10), // soft before hard kill
		"IOWeight=100",                             // default when unset
		"TasksMax=512",                             // default when unset
		"Restart=on-failure",
		"RestartSec=5s",
		"StartLimitBurst=3",
		"StartLimitIntervalSec=60",
	} {
		if !strings.Contains(u, want+"\n") {
			t.Errorf("unit file missing %q\n---\n%s", want, u)
		}
	}
	// Volume binds sorted into BindPaths (host:in-unit pairs).
	if !strings.Contains(u, "BindPaths=/var/lib/expanse/volumes/vol-a/mnt:/var/lib/db /var/lib/expanse/volumes/vol-b/mnt:/var/lib/postgresql\n") {
		t.Errorf("BindPaths not sorted/present:\n%s", u)
	}
	// Volume mounts sorted into ReadWritePaths.
	if !strings.Contains(u, "ReadWritePaths=/persist/expanse/blocks/web/a /persist/expanse/blocks/web/z") {
		t.Errorf("ReadWritePaths not sorted/present:\n%s", u)
	}
	// Credentials via LoadCredential.
	if !strings.Contains(u, "LoadCredential=db_password:/persist/expanse/secrets/db_password\n") {
		t.Errorf("LoadCredential missing:\n%s", u)
	}
	if strings.Contains(u, "Environment=") || strings.Contains(u, "EnvironmentFile=") {
		t.Error("secrets must never leak via env vars")
	}
	if strings.Contains(u, "/nix/store") {
		t.Error("secrets must never live in /nix/store")
	}
}

// Sandboxing flags present regardless of block type / spec shape.
func TestSandboxingAlwaysPresent(t *testing.T) {
	for _, s := range []Spec{
		{Name: "a", Namespace: "default"},
		{Name: "b", Namespace: "prod", Limits: &pb.ResourcePair{Cpu: "500m", Memory: "256Mi"}},
		{Name: "c", Namespace: "default", StaticUID: 990, VolumeMounts: []string{"/data"}},
	} {
		u := UnitFile(s)
		for _, want := range []string{
			"NoNewPrivileges=yes", "PrivateTmp=yes", "ProtectSystem=strict",
			"ProtectHome=yes", "SystemCallFilter=@system-service",
			"RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6",
		} {
			if !strings.Contains(u, want+"\n") {
				t.Errorf("spec %+v missing %q", s, want)
			}
		}
	}
	// Static uid path.
	if u := UnitFile(Spec{Namespace: "d", Name: "x", StaticUID: 990}); !strings.Contains(u, "DynamicUser=no") {
		t.Errorf("static uid spec missing DynamicUser=no:\n%s", u)
	}
	// Dynamic user default.
	if u := UnitFile(Spec{Namespace: "d", Name: "y"}); !strings.Contains(u, "DynamicUser=yes") {
		t.Errorf("default spec missing DynamicUser=yes:\n%s", u)
	}
}

// StaticUID must actually pin the unit's identity, not just disable
// DynamicUser: a previously dormant bug (nothing set StaticUID before
// share/smb) left it computed but never emitted, so a pinned uid was
// silently ignored and the unit ran as whatever User= defaults to.
func TestStaticUIDEmitsUser(t *testing.T) {
	u := UnitFile(Spec{Namespace: "d", Name: "z", StaticUID: 990})
	if !strings.Contains(u, "User=990\n") {
		t.Errorf("StaticUID=990 did not emit User=990:\n%s", u)
	}
}

// RunAsRoot disables DynamicUser without pinning a uid (root is
// systemd's own default when User= is left unset) — the escape hatch
// share/smb and share/nfs need: smbd/nfsd must setuid()/setgid() to the
// connecting or exported user, which requires the capability, not a
// directory permission.
func TestRunAsRootDisablesDynamicUser(t *testing.T) {
	u := UnitFile(Spec{Namespace: "d", Name: "z", RunAsRoot: true})
	if !strings.Contains(u, "DynamicUser=no\n") {
		t.Errorf("RunAsRoot missing DynamicUser=no:\n%s", u)
	}
	if strings.Contains(u, "\nUser=") {
		t.Errorf("RunAsRoot must not pin a uid (root is the unset default):\n%s", u)
	}
}

// Unit naming (§5.3).
func TestUnitNaming(t *testing.T) {
	if got := UnitName("default", "web", 2); got != "expanse-block@default-web-2.service" {
		t.Errorf("UnitName = %q", got)
	}
	if got := JournaldIdentifier("default", "web", 2); got != "expanse-block-default-web-2" {
		t.Errorf("JournaldIdentifier = %q", got)
	}
}

// UnitNameForSpec routes RunAsRoot replicas to the dedicated static
// template (nix/modules/agent.nix's expanse-block-root@.service) and
// everything else to the ordinary one, unchanged.
func TestUnitNameForSpecRoutesRunAsRoot(t *testing.T) {
	ordinary := Spec{Namespace: "default", Name: "web", Index: 2}
	if got := UnitNameForSpec(ordinary); got != UnitName("default", "web", 2) {
		t.Errorf("UnitNameForSpec(ordinary) = %q, want %q", got, UnitName("default", "web", 2))
	}
	root := Spec{Namespace: "default", Name: "share", Index: 0, RunAsRoot: true}
	want := "expanse-block-root@default-share-0.service"
	if got := UnitNameForSpec(root); got != want {
		t.Errorf("UnitNameForSpec(root) = %q, want %q", got, want)
	}
}

// The template file must not embed the replica index — that is what lets
// a replicas-only change hit the closure cache.
func TestTemplateHasNoIndex(t *testing.T) {
	a := UnitFile(sampleSpec())
	b := sampleSpec()
	b.Index = 7
	if a != UnitFile(b) {
		t.Error("unit file content varies with replica index; instances would rebuild")
	}
	if hash := ClosureHash(sampleSpec()); hash != ClosureHash(b) {
		t.Error("closure hash varies with replica index")
	}
}

// mockBuilder counts build invocations.
type mockBuilder struct {
	builds int
	fail   bool
}

func (m *mockBuilder) Build(ctx context.Context, blockType string) (nix.StorePath, error) {
	m.builds++
	if m.fail {
		return "", fmt.Errorf("build failed")
	}
	return "/nix/store/test-block-closure", nil
}

func newTestCache(t *testing.T) *Cache {
	t.Helper()
	c, err := NewCache(filepath.Join(t.TempDir(), "closure-cache.json"))
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	return c
}

// Cache short-circuit: zero build invocations on a second apply with an
// unchanged closure hash.
func TestCacheShortCircuit(t *testing.T) {
	ctx := context.Background()
	cache := newTestCache(t)
	mb := &mockBuilder{}
	a := &Applier{Cache: cache, Builder: mb}

	r1, err := a.Apply(ctx, sampleSpec())
	if err != nil || !r1.Built {
		t.Fatalf("first apply = %+v err %v, want built", r1, err)
	}
	r2, err := a.Apply(ctx, sampleSpec())
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if r2.Built || mb.builds != 1 {
		t.Errorf("second apply built again (built=%v builds=%d) — cache miss on unchanged hash", r2.Built, mb.builds)
	}
	if r2.StorePath != r1.StorePath {
		t.Errorf("cache path mismatch: %q vs %q", r2.StorePath, r1.StorePath)
	}
}

// Replicas-only change path completes without invoking the build step,
// and fast (< 5 s, §5.3).
func TestReplicasOnlyChangeSkipsBuild(t *testing.T) {
	ctx := context.Background()
	cache := newTestCache(t)
	mb := &mockBuilder{}
	a := &Applier{Cache: cache, Builder: mb}

	if _, err := a.Apply(ctx, sampleSpec()); err != nil {
		t.Fatalf("initial apply: %v", err)
	}
	// "Scale up": index 1 of the same block — same closure inputs.
	start := time.Now()
	scale := sampleSpec()
	scale.Index = 1
	res, err := a.Apply(ctx, scale)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("scale apply: %v", err)
	}
	if res.Built || mb.builds != 1 {
		t.Errorf("replicas-only change rebuilt (built=%v builds=%d)", res.Built, mb.builds)
	}
	if elapsed > 5*time.Second {
		t.Errorf("replicas-only change took %v, want < 5s", elapsed)
	}
}

// Cache persists across "restarts" (new Cache instance over the file).
func TestCachePersistence(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "closure-cache.json")
	c1, _ := NewCache(path)
	mb := &mockBuilder{}
	a1 := &Applier{Cache: c1, Builder: mb}
	if _, err := a1.Apply(ctx, sampleSpec()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	c2, err := NewCache(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	a2 := &Applier{Cache: c2, Builder: mb}
	if _, err := a2.Apply(ctx, sampleSpec()); err != nil {
		t.Fatalf("apply after restart: %v", err)
	}
	if mb.builds != 1 {
		t.Errorf("rebuilt after restart: %d builds", mb.builds)
	}
}

// fakeUnitAPI records unit operations.
type fakeUnitAPI struct {
	states map[string][3]string
	starts int
	status map[string]string // unit -> StatusText
}

func (f *fakeUnitAPI) StatusText(ctx context.Context, unit string) (string, error) {
	return f.status[unit], nil
}

func (f *fakeUnitAPI) UnitState(ctx context.Context, unit string) (string, string, string, error) {
	if s, ok := f.states[unit]; ok {
		return s[0], s[1], s[2], nil
	}
	return "not-found", "inactive", "dead", nil
}

func (f *fakeUnitAPI) Start(ctx context.Context, unit string) error {
	f.starts++
	f.states[unit] = [3]string{"loaded", "active", "running"}
	return nil
}

func (f *fakeUnitAPI) Stop(ctx context.Context, unit string) error {
	f.states[unit] = [3]string{"loaded", "inactive", "dead"}
	return nil
}

// The reconcile manager: out-of-sync replica → closure + start, using the
// cache; switch only fires after a real build.
func TestManagerConverges(t *testing.T) {
	ctx := context.Background()
	api := &fakeUnitAPI{states: map[string][3]string{}}
	mb := &mockBuilder{}
	switches := 0
	m := NewManager(api, &Applier{Cache: newTestCache(t), Builder: mb})
	m.SpecDir = t.TempDir()
	m.Switch = func(ctx context.Context, p string) error { switches++; return nil }

	specBytes, err := json.Marshal(sampleSpec())
	if err != nil {
		t.Fatal(err)
	}
	res, err := m.Load("block-replica:default/web/0", specBytes)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Pass 1: not found → build + switch + start.
	o1, err := m.Observe(ctx, res)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if o1.InSync {
		t.Fatal("first observe reported in-sync")
	}
	acts, err := m.Plan(ctx, res, o1)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(acts) != 2 {
		t.Fatalf("got %d actions, want 2 (closure, start)", len(acts))
	}
	for _, a := range acts {
		if err := m.Apply(ctx, a); err != nil {
			t.Fatalf("Apply %s: %v", a.Kind, err)
		}
	}
	if mb.builds != 1 || switches != 1 || api.starts != 1 {
		t.Errorf("pass1: builds=%d switches=%d starts=%d", mb.builds, switches, api.starts)
	}

	// Pass 2: in sync, closure cached → no build, no switch, no start.
	o2, err := m.Observe(ctx, res)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if !o2.InSync {
		t.Fatalf("second observe not in sync: %+v", o2)
	}
	acts2, err := m.Plan(ctx, res, o2)
	if err != nil {
		t.Fatalf("Plan2: %v", err)
	}
	if len(acts2) != 1 {
		t.Fatalf("pass2 has %d actions, want 1 (closure only, cached)", len(acts2))
	}
	if err := m.Apply(ctx, acts2[0]); err != nil {
		t.Fatalf("Apply pass2: %v", err)
	}
	if mb.builds != 1 || switches != 1 || api.starts != 1 {
		t.Errorf("pass2 touched the system: builds=%d switches=%d starts=%d", mb.builds, switches, api.starts)
	}
}

// The manager writes the per-replica spec JSON for expanse-block-run
// before starting the unit (the static template only carries %i), and
// Delete stops the unit + removes the spec file.
func TestManagerSpecFileLifecycle(t *testing.T) {
	ctx := context.Background()
	api := &fakeUnitAPI{states: map[string][3]string{}}
	mb := &mockBuilder{}
	m := NewManager(api, &Applier{Cache: newTestCache(t), Builder: mb})
	m.SpecDir = t.TempDir()

	specBytes, err := json.Marshal(sampleSpec())
	if err != nil {
		t.Fatal(err)
	}
	res, err := m.Load("block-replica:default/web/0", specBytes)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	o, err := m.Observe(ctx, res)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	acts, err := m.Plan(ctx, res, o)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for _, a := range acts {
		if err := m.Apply(ctx, a); err != nil {
			t.Fatalf("Apply %s: %v", a.Kind, err)
		}
	}
	specPath := filepath.Join(m.SpecDir,
		Instance(sampleSpec().Namespace, sampleSpec().Name, sampleSpec().Index)+".json")
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("spec file missing after converge: %v", err)
	}
	var s Spec
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("bad spec file: %v", err)
	}
	if s.Type != sampleSpec().Type || s.Index != sampleSpec().Index {
		t.Errorf("spec file content wrong: %+v", s)
	}

	// Delete: unit stopped, spec file gone.
	if err := m.Delete(ctx, res); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(specPath); !os.IsNotExist(err) {
		t.Errorf("spec file survived Delete: %v", err)
	}
	if api.states[UnitName("default", "web", 0)][1] != "inactive" {
		t.Errorf("unit not stopped: %v", api.states[UnitName("default", "web", 0)])
	}
}

// converged loads spec and applies the manager's plan, so the next Observe sees a running unit.
func converged(t *testing.T, api *fakeUnitAPI, spec Spec) (*Manager, reconcile.Resource) {
	t.Helper()
	ctx := context.Background()
	m := NewManager(api, &Applier{Cache: newTestCache(t), Builder: &mockBuilder{}})
	m.SpecDir = t.TempDir()
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	res, err := m.Load("block-replica:default/x/0", b)
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
			t.Fatal(err)
		}
	}
	return m, res
}

// A VM replica's unit is running while its guest boots: in sync and healthy, but not ready.
func TestVMReplicaReadinessFollowsTheGuest(t *testing.T) {
	ctx := context.Background()
	spec := sampleSpec()
	spec.Type, spec.RunAsRoot = "vm/instance", true
	api := &fakeUnitAPI{states: map[string][3]string{}, status: map[string]string{}}
	m, res := converged(t, api, spec)
	unit := UnitNameForSpec(spec)

	for text, want := range map[string]bool{
		"booting":                              false,
		"not ready: guest in emergency.target": false,
		"ready: multi-user.target reached":     true,
	} {
		api.status[unit] = text
		o, err := m.Observe(ctx, res)
		if err != nil {
			t.Fatal(err)
		}
		if !o.InSync || o.Health != reconcile.HealthHealthy {
			t.Errorf("%q: in_sync=%v health=%v, want in sync and healthy", text, o.InSync, o.Health)
		}
		if o.Ready == nil || *o.Ready != want {
			t.Errorf("%q: ready=%v, want %v", text, o.Ready, want)
		}
		if o.Details["status"] != text {
			t.Errorf("%q: details status = %q", text, o.Details["status"])
		}
	}
}

func TestOtherReplicasReportNoReadiness(t *testing.T) {
	api := &fakeUnitAPI{states: map[string][3]string{}, status: map[string]string{}}
	m, res := converged(t, api, sampleSpec())
	o, err := m.Observe(context.Background(), res)
	if err != nil {
		t.Fatal(err)
	}
	if o.Ready != nil {
		t.Errorf("ready = %v, want unreported", *o.Ready)
	}
}
