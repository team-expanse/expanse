package fileman

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/expanse/expanse/internal/reconcile"
)

func mustLoad(t *testing.T, m reconcile.Manager, id, spec string) reconcile.Resource {
	t.Helper()
	r, err := m.Load(id, []byte(spec))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return r
}

func observe(t *testing.T, m reconcile.Manager, r reconcile.Resource) reconcile.Observed {
	t.Helper()
	o, err := m.Observe(context.Background(), r)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	return o
}

func plan(t *testing.T, m reconcile.Manager, r reconcile.Resource, o reconcile.Observed) []reconcile.Action {
	t.Helper()
	acts, err := m.Plan(context.Background(), r, o)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return acts
}

func applyAll(t *testing.T, m reconcile.Manager, acts []reconcile.Action) {
	t.Helper()
	for _, a := range acts {
		if err := m.Apply(context.Background(), a); err != nil {
			t.Fatalf("Apply %s: %v", a.Description, err)
		}
	}
}

func TestFileConvergeContentModeAndIdempotency(t *testing.T) {
	m := New().FileManager()
	path := filepath.Join(t.TempDir(), "motd")
	spec := fmt.Sprintf("path: %s\ncontent: hello\nmode: \"0644\"\n", path)
	r := mustLoad(t, m, "file:"+path, spec)

	// 1. Create.
	o := observe(t, m, r)
	if o.Exists || o.InSync {
		t.Fatalf("fresh observe: exists=%v in_sync=%v, want false/false", o.Exists, o.InSync)
	}
	acts := plan(t, m, r, o)
	if len(acts) != 1 {
		t.Fatalf("plan on missing file: %d actions, want 1", len(acts))
	}
	applyAll(t, m, acts)

	// 2. Now in sync; re-plan must be empty (idempotency).
	o = observe(t, m, r)
	if !o.InSync {
		t.Fatalf("after apply: in_sync=%v details=%v", o.InSync, o.Details)
	}
	if acts := plan(t, m, r, o); len(acts) != 0 {
		t.Errorf("in-sync resource planned %d actions, want 0", len(acts))
	}

	// 3. Content drift detected and repaired.
	if err := os.WriteFile(path, []byte("bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	o = observe(t, m, r)
	acts = plan(t, m, r, o)
	if len(acts) != 1 || acts[0].Kind != "create" {
		t.Fatalf("content drift: got %v", acts)
	}
	applyAll(t, m, acts)
	if data, _ := os.ReadFile(path); string(data) != "hello" {
		t.Errorf("content = %q, want hello", data)
	}

	// 4. Mode drift detected and repaired.
	if err := os.Chmod(path, 0o777); err != nil {
		t.Fatal(err)
	}
	o = observe(t, m, r)
	acts = plan(t, m, r, o)
	if len(acts) != 1 || acts[0].Kind != "update" {
		t.Fatalf("mode drift: got %v", acts)
	}
	applyAll(t, m, acts)
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o644 {
		t.Errorf("mode = %04o, want 0644", st.Mode().Perm())
	}

	// 5. Delete the resource's file: back to create plan.
	os.Remove(path)
	o = observe(t, m, r)
	if o.Exists {
		t.Error("removed file still observed as existing")
	}
}

func TestAtomicWriteLeavesNoTempOnFailure(t *testing.T) {
	dir := t.TempDir()
	// Make the target path's directory read-only after creating the temp
	// file fails differently — simulate failure by pointing the write at a
	// path inside a file (not a directory).
	notADir := filepath.Join(dir, "occupied")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(notADir, "sub", "file") // ENOTDIR on CreateTemp
	if err := atomicWrite(target, []byte("data"), 0o644); err == nil {
		t.Fatal("expected failure writing under a non-directory")
	}
	// No temp files left behind anywhere.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), "expanse-tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestDirectoryManager(t *testing.T) {
	m := New().DirManager()
	path := filepath.Join(t.TempDir(), "data", "sub")
	spec := fmt.Sprintf("path: %s\nmode: \"0700\"\n", path)
	r := mustLoad(t, m, "dir:"+path, spec)

	o := observe(t, m, r)
	if o.Exists {
		t.Fatal("fresh dir observe: exists, want false")
	}
	applyAll(t, m, plan(t, m, r, o))
	o = observe(t, m, r)
	if !o.InSync {
		t.Fatalf("after mkdir: %+v", o)
	}
	if acts := plan(t, m, r, o); len(acts) != 0 {
		t.Errorf("in-sync dir planned %d actions", len(acts))
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	o = observe(t, m, r)
	acts := plan(t, m, r, o)
	if len(acts) != 1 || acts[0].Kind != "update" {
		t.Fatalf("mode drift: %v", acts)
	}
	applyAll(t, m, acts)
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o700 {
		t.Errorf("mode = %04o, want 0700", st.Mode().Perm())
	}
}

func TestBadSpecRejected(t *testing.T) {
	m := New().FileManager()
	if _, err := m.Load("file:x", []byte("content: hi\n")); err == nil {
		t.Error("spec without path must be rejected")
	}
	if _, err := m.Load("file:x", []byte("path: /x\nmode: \"0999\"\n")); err == nil {
		t.Error("invalid octal mode must be rejected at load/observe")
	} else {
		// Mode error surfaces at Observe; Load succeeds here, so only check
		// the missing-path case strictly and that observe fails.
		r, _ := m.Load("file:x", []byte("path: /x\nmode: \"0999\"\n"))
		if _, err := m.Observe(context.Background(), r); err == nil {
			t.Error("invalid octal mode must fail observe")
		}
	}
}
