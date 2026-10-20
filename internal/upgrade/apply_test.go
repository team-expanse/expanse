package upgrade

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type fakeNix struct {
	calls     []string
	buildErr  error
	switchErr error
	built     string
	subst     string
}

func (f *fakeNix) BuildSystem(_ context.Context, file, substituter string, _ io.Writer) (string, error) {
	f.calls = append(f.calls, "build "+filepath.Base(filepath.Dir(filepath.Dir(file))))
	f.subst = substituter
	if f.buildErr != nil {
		return "", f.buildErr
	}
	return f.built, nil
}

func (f *fakeNix) SetProfile(_ context.Context, path string) error {
	f.calls = append(f.calls, "profile "+path)
	return nil
}

func (f *fakeNix) Switch(_ context.Context, path string) error {
	f.calls = append(f.calls, "switch "+path)
	return f.switchErr
}

type applyRig struct {
	u     *Upgrader
	nix   *fakeNix
	nixos string
}

func newApplyRig(t *testing.T) *applyRig {
	t.Helper()
	r := newTestRelease(t, "1.12.0", map[string][]byte{"expanse-1.12.0.tar.gz": mustRead(t, writeTarball(t, release("1.12.0")))})
	nixos := t.TempDir()
	os.MkdirAll(filepath.Join(nixos, "expanse/nix"), 0o755)
	os.WriteFile(filepath.Join(nixos, "expanse/nix/version.nix"), []byte(`"1.11.3"`), 0o644)
	nx := &fakeNix{built: "/nix/store/abc-nixos-system"}
	return &applyRig{
		nix: nx, nixos: nixos,
		u: &Upgrader{Source: r.source(t), Tree: &Tree{Dir: nixos}, Nix: nx, StatusPath: filepath.Join(t.TempDir(), "upgrade.json")},
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestApplyBuildsTheNewSourceThenSetsTheProfileAndSwitches(t *testing.T) {
	r := newApplyRig(t)
	if err := r.u.Apply(context.Background(), "1.12.0", io.Discard); err != nil {
		t.Fatal(err)
	}
	want := []string{"build expanse", "profile /nix/store/abc-nixos-system", "switch /nix/store/abc-nixos-system"}
	if !reflect.DeepEqual(r.nix.calls, want) {
		t.Errorf("calls = %v, want %v", r.nix.calls, want)
	}
	st, err := ReadStatus(r.u.StatusPath)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != StateDone || st.Version != "1.12.0" || st.From != "1.11.3" || st.System != "/nix/store/abc-nixos-system" {
		t.Errorf("status = %+v", st)
	}
}

func TestApplyPutsTheOldSourceBackWhenTheBuildFails(t *testing.T) {
	r := newApplyRig(t)
	r.nix.buildErr = errors.New("builder failed")
	if err := r.u.Apply(context.Background(), "1.12.0", io.Discard); err == nil {
		t.Fatal("a failed build was reported as success")
	}
	if v, _ := r.u.Tree.Version("expanse"); v != "1.11.3" {
		t.Errorf("source after a failed build is %q", v)
	}
	st, _ := ReadStatus(r.u.StatusPath)
	if st.State != StateFailed || st.Error == "" {
		t.Errorf("status = %+v", st)
	}
	for _, c := range r.nix.calls {
		if c != "build expanse" {
			t.Errorf("went on after the failed build: %v", r.nix.calls)
		}
	}
}

func TestApplyReportsAFailedSwitch(t *testing.T) {
	r := newApplyRig(t)
	r.nix.switchErr = errors.New("unit failed")
	if err := r.u.Apply(context.Background(), "1.12.0", io.Discard); err == nil {
		t.Fatal("a failed switch was reported as success")
	}
	st, _ := ReadStatus(r.u.StatusPath)
	if st.State != StateFailed {
		t.Errorf("status = %+v", st)
	}
}

func TestApplyUsesTheReleaseCacheAsASubstituter(t *testing.T) {
	r := newApplyRig(t)
	cache := writeTarball(t, map[string]string{"nix-cache-info": "StoreDir: /nix/store\n"})
	rel := newTestRelease(t, "1.12.0", map[string][]byte{
		"expanse-1.12.0.tar.gz": mustRead(t, writeTarball(t, release("1.12.0"))),
		CacheName("1.12.0"):     mustRead(t, cache),
	})
	r.u.Source = rel.source(t)
	if err := r.u.Apply(context.Background(), "1.12.0", io.Discard); err != nil {
		t.Fatal(err)
	}
	dir, ok := cutPrefix(r.nix.subst, "file://")
	if !ok {
		t.Fatalf("substituter = %q", r.nix.subst)
	}
	if _, err := os.Stat(filepath.Join(dir, "nix-cache-info")); err != nil {
		t.Errorf("the cache was not unpacked where the substituter points: %v", err)
	}
}

func cutPrefix(s, p string) (string, bool) {
	if len(s) < len(p) || s[:len(p)] != p {
		return "", false
	}
	return s[len(p):], true
}
