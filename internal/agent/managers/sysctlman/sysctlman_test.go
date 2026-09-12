package sysctlman

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/expanse/expanse/internal/reconcile"
)

// fakeProcSys creates a fake /proc/sys tree and returns a key whose value
// can be read/written like a real sysctl.
func fakeProcSys(t *testing.T, key, initial string) string {
	t.Helper()
	root := t.TempDir()
	procRoot = filepath.Join(root, "proc", "sys")
	p := procPath(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}
	return key
}

func loadRes(t *testing.T, key, value string) reconcile.Resource {
	t.Helper()
	m := New()
	r, err := m.Load("sysctl:"+key, []byte(fmt.Sprintf("key: %s\nvalue: %s\n", key, value)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return r
}

func TestSysctlConvergeAndIdempotency(t *testing.T) {
	key := fakeProcSys(t, "net/core/somaxconn", "128\n")
	m := New()
	r := loadRes(t, key, "4096")

	o, err := m.Observe(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if o.InSync || o.Details["value"] != "128" {
		t.Fatalf("initial observe: %+v", o)
	}
	acts, err := m.Plan(context.Background(), r, o)
	if err != nil || len(acts) != 1 {
		t.Fatalf("plan: %v %v", acts, err)
	}
	if err := m.Apply(context.Background(), acts[0]); err != nil {
		t.Fatal(err)
	}

	// Converged: observe in sync, plan empty (idempotency).
	o, err = m.Observe(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if !o.InSync || o.Health != reconcile.HealthHealthy {
		t.Fatalf("after apply: %+v", o)
	}
	if acts, _ := m.Plan(context.Background(), r, o); len(acts) != 0 {
		t.Errorf("in-sync sysctl planned %d actions", len(acts))
	}
}

func TestSysctlUnknownKeyObservedNotExists(t *testing.T) {
	m := New()
	r := loadRes(t, "net/core/definitely-not-a-sysctl", "1")
	o, err := m.Observe(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if o.Exists {
		t.Errorf("unknown sysctl should not exist: %+v", o)
	}
}

func TestSysctlWhitespaceNormalization(t *testing.T) {
	// Some sysctls return tab-separated lists; the manager normalizes both
	// sides before comparing.
	key := fakeProcSys(t, "net/ipv4/tcp_rmem", "4096\t87380\t2974968")
	m := New()
	r := loadRes(t, key, "4096 87380 2974968")
	o, err := m.Observe(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if !o.InSync {
		t.Errorf("tab/space normalization failed: %+v", o)
	}
}

func TestBadSpec(t *testing.T) {
	m := New()
	if _, err := m.Load("sysctl:x", []byte("value: 1\n")); err == nil {
		t.Error("missing key must fail load")
	}
	if _, err := m.Load("sysctl:x", []byte("key: a.b\n")); err == nil {
		t.Error("missing value must fail load")
	}
}
