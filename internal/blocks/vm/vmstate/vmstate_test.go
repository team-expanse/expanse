package vmstate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTokenIsEmptyUntilAStateIsCommitted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "default-vm-0")
	if got := Token(dir); got != "" {
		t.Fatalf("Token of a missing dir = %q", got)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(TempPath(dir), []byte("ram"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Token(dir); got != "" {
		t.Errorf("an uncommitted save has token %q", got)
	}
	if err := Commit(dir, "r42"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got := Token(dir); got != "r42" {
		t.Errorf("Token = %q, want r42", got)
	}
	if raw, err := os.ReadFile(StatePath(dir)); err != nil || string(raw) != "ram" {
		t.Errorf("state file = %q, %v", raw, err)
	}
}

func TestCommitReplacesAnOlderSave(t *testing.T) {
	dir := t.TempDir()
	for _, tok := range []string{"r1", "r2"} {
		if err := os.WriteFile(TempPath(dir), []byte(tok), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := Commit(dir, tok); err != nil {
			t.Fatalf("Commit %s: %v", tok, err)
		}
	}
	raw, _ := os.ReadFile(StatePath(dir))
	if Token(dir) != "r2" || string(raw) != "r2" {
		t.Errorf("token %q state %q, want r2 r2", Token(dir), raw)
	}
}

func TestATokenWithoutItsStateIsNoSave(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("r9"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Token(dir); got != "" {
		t.Errorf("Token = %q with no state file", got)
	}
}

func TestRemoveDeletesTheSave(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "x")
	_ = os.MkdirAll(dir, 0o700)
	_ = os.WriteFile(TempPath(dir), []byte("ram"), 0o600)
	_ = Commit(dir, "r1")
	if err := Remove(dir); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("dir still there: %v", err)
	}
	if err := Remove(dir); err != nil {
		t.Errorf("Remove of a missing dir: %v", err)
	}
}

func TestDirIsPerInstanceUnderTheBase(t *testing.T) {
	if got := Dir("", "default-vm-0"); got != "/persist/expanse/vm-state/default-vm-0" {
		t.Errorf("Dir = %q", got)
	}
	if got := Dir("/x", "a-b-1"); got != "/x/a-b-1" {
		t.Errorf("Dir = %q", got)
	}
}

func TestTheShapeLivesAndDiesWithTheSave(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "default-vm1-0")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteShape(dir, []byte(`{"cpus":2}`)); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(ShapePath(dir)); err != nil || string(raw) != `{"cpus":2}` {
		t.Fatalf("shape = %q, %v", raw, err)
	}
	if err := Remove(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ShapePath(dir)); !os.IsNotExist(err) {
		t.Errorf("the shape outlived its save: %v", err)
	}
}
