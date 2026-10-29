package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func writeSpec(t *testing.T, dir string, s spec) {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "default-vm1-0.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func vmArgs(cpu string) []string {
	return []string{"--config", `{"machine":"pc"}`, "--cpu", cpu, "--mem", "1Gi"}
}

func TestAReloadAppliesOnlyALiveChange(t *testing.T) {
	dir := t.TempDir()
	var applied [][]string
	apply := func(cur, next []string) error { applied = append(applied, next); return nil }

	writeSpec(t, dir, spec{Type: "vm/instance", Args: vmArgs("2")})
	got, err := reloadFrom(dir, "default-vm1-0", "vm/instance", vmArgs("1"), apply)
	if err != nil || !slices.Equal(got, vmArgs("2")) || len(applied) != 1 {
		t.Fatalf("live change: args=%v err=%v applied=%v", got, err, applied)
	}

	if _, err := reloadFrom(dir, "default-vm1-0", "vm/instance", vmArgs("2"), apply); err != nil || len(applied) != 1 {
		t.Errorf("an unchanged spec: err=%v, applied %d times", err, len(applied))
	}

	other := vmArgs("2")
	other[1] = `{"machine":"q35"}`
	writeSpec(t, dir, spec{Type: "vm/instance", Args: other})
	if _, err := reloadFrom(dir, "default-vm1-0", "vm/instance", vmArgs("2"), apply); err == nil || len(applied) != 1 {
		t.Errorf("a config change: err=%v, applied %d times", err, len(applied))
	}

	writeSpec(t, dir, spec{Type: "vm/instance", Args: vmArgs("4"), Stopped: true})
	if _, err := reloadFrom(dir, "default-vm1-0", "vm/instance", vmArgs("2"), apply); err == nil {
		t.Error("a suspend was taken as a live change")
	}
}

func TestAFailedReloadRestartsTheUnit(t *testing.T) {
	dir, live := t.TempDir(), t.TempDir()
	writeSpec(t, dir, spec{Type: "vm/instance", Args: vmArgs("2")})
	var restarted []string
	r := reloader{
		specDir: dir, liveDir: live, instance: "default-vm1-0", typ: "vm/instance",
		apply:   func(cur, next []string) error { return errors.New("no hotplug") },
		restart: func(instance string) error { restarted = append(restarted, instance); return nil },
	}
	hup := make(chan os.Signal, 1)
	done := make(chan struct{})
	go func() { r.serve(context.Background(), vmArgs("1"), hup); close(done) }()

	marker := filepath.Join(live, "default-vm1-0")
	deadline := time.Now().Add(5 * time.Second)
	for _, err := os.Stat(marker); err != nil; _, err = os.Stat(marker) {
		if time.Now().After(deadline) {
			t.Fatal("no reload marker")
		}
		time.Sleep(10 * time.Millisecond)
	}
	hup <- os.Interrupt
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("still serving after a failed reload")
	}
	if !slices.Equal(restarted, []string{"default-vm1-0"}) {
		t.Errorf("restarted %v", restarted)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("marker kept after serving stopped: %v", err)
	}
}

func TestUnitFromCgroup(t *testing.T) {
	got := unitFromCgroup("0::/expanse-blocks.slice/expanse-block-root@default-vm1-0.service\n")
	if got != "expanse-block-root@default-vm1-0.service" {
		t.Errorf("unit = %q", got)
	}
	if got := unitFromCgroup("0::/user.slice\n"); got != "" {
		t.Errorf("a slice read as a unit: %q", got)
	}
}
