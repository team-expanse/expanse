package nixman

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/agent/nix"
)

type fakeDriver struct {
	current  nix.StorePath
	built    nix.StorePath
	switched []nix.StorePath
}

func (d *fakeDriver) Build(ctx context.Context, ref, attr string, logs io.Writer) (nix.StorePath, error) {
	d.built = "/nix/store/built-system"
	return d.built, nil
}

func (d *fakeDriver) Switch(ctx context.Context, p nix.StorePath, mode nix.SwitchMode) error {
	d.switched = append(d.switched, p)
	d.current = p
	return nil
}

func (d *fakeDriver) CurrentSystem(ctx context.Context) (nix.StorePath, error)  { return d.current, nil }
func (d *fakeDriver) Generations(ctx context.Context) ([]nix.Generation, error) { return nil, nil }
func (d *fakeDriver) Rollback(ctx context.Context, n int) error                 { return nil }
func (d *fakeDriver) GC(ctx context.Context, keep time.Duration) error          { return nil }

func TestNixConfigConvergeAndIdempotency(t *testing.T) {
	drv := &fakeDriver{current: "/nix/store/old-system"}
	markers := filepath.Join(t.TempDir(), "markers")
	m := NewWithMarkerDir(drv, markers, nil)

	r, err := m.Load("nix-config:main", []byte("flake: path:/etc/nixos\nattr: toplevel\n"))
	if err != nil {
		t.Fatal(err)
	}

	// First tick: out of sync (never switched), plan builds + switches.
	o, err := m.Observe(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if o.InSync {
		t.Fatalf("never-switched nix-config must be out of sync: %+v", o)
	}
	acts, err := m.Plan(context.Background(), r, o)
	if err != nil || len(acts) != 1 {
		t.Fatalf("plan: %v %v", acts, err)
	}
	if !acts[0].Destructive {
		t.Error("nix switch action must be marked destructive")
	}
	if err := m.Apply(context.Background(), acts[0]); err != nil {
		t.Fatal(err)
	}
	if len(drv.switched) != 1 || drv.switched[0] != "/nix/store/built-system" {
		t.Errorf("switched = %v", drv.switched)
	}

	// Second observe: in sync, plan empty (idempotency).
	o, err = m.Observe(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if !o.InSync {
		t.Fatalf("after switch: %+v", o)
	}
	if acts, _ := m.Plan(context.Background(), r, o); len(acts) != 0 {
		t.Errorf("in-sync nix-config planned %d actions", len(acts))
	}

	// Someone else switches the system (drift): out of sync again.
	drv.current = "/nix/store/other-system"
	o, _ = m.Observe(context.Background(), r)
	if o.InSync {
		t.Error("system drift must be detected")
	}
}

func TestNixConfigBadSpec(t *testing.T) {
	m := NewWithMarkerDir(&fakeDriver{}, t.TempDir(), nil)
	cases := []string{
		"attr: x\n",  // missing flake
		"flake: f\n", // missing attr
		"flake: f\nattr: a\nswitch_mode: reboot\n", // bad mode
	}
	for i, spec := range cases {
		if _, err := m.Load("nix-config:x", []byte(spec)); err == nil {
			t.Errorf("case %d: expected error", i)
		}
	}
}

func TestNixConfigMarkerSurvivesRestart(t *testing.T) {
	drv := &fakeDriver{current: "/nix/store/built-system"}
	markers := filepath.Join(t.TempDir(), "markers")
	m1 := NewWithMarkerDir(drv, markers, nil)
	r, _ := m1.Load("nix-config:main", []byte("flake: f\nattr: a\n"))
	o, _ := m1.Observe(context.Background(), r)
	acts, _ := m1.Plan(context.Background(), r, o)
	m1.Apply(context.Background(), acts[0])
	// New manager instance (agent restart) with same marker dir:
	m2 := NewWithMarkerDir(drv, markers, nil)
	o, err := m2.Observe(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if !o.InSync {
		t.Errorf("after restart: %+v — marker must persist", o)
	}
	if _, err := os.Stat(filepath.Join(markers, "nix-config_main")); err != nil {
		t.Errorf("marker file: %v", err)
	}
}
