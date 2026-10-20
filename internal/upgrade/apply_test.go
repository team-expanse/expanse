package upgrade

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type fakeNix struct {
	calls     []string
	buildErr  error
	switchErr error
	built     string
	subst     string
	substInfo error // stat of the substituter's nix-cache-info at build time
}

func (f *fakeNix) BuildSystem(_ context.Context, file, substituter string, _ io.Writer) (string, error) {
	f.calls = append(f.calls, "build "+filepath.Base(filepath.Dir(filepath.Dir(file))))
	f.subst = substituter
	_, f.substInfo = os.Stat(filepath.Join(strings.TrimPrefix(substituter, "file://"), "nix-cache-info"))
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
	nx := &fakeNix{built: fakeSystem(t, "1.12.0")}
	return &applyRig{
		nix: nx, nixos: nixos,
		u: &Upgrader{Source: r.source(t), Tree: &Tree{Dir: nixos}, Nix: nx, StatusPath: filepath.Join(t.TempDir(), "upgrade.json")},
	}
}

// fakeSystem is a built system whose sw/bin/expanse comes from expanse-<version>.
func fakeSystem(t *testing.T, version string) string {
	t.Helper()
	store := t.TempDir()
	bin := filepath.Join(store, "abc-expanse-"+version, "bin")
	sys := filepath.Join(store, "def-nixos-system")
	os.MkdirAll(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "expanse"), nil, 0o755)
	os.MkdirAll(filepath.Join(sys, "sw/bin"), 0o755)
	if err := os.Symlink(filepath.Join(bin, "expanse"), filepath.Join(sys, "sw/bin/expanse")); err != nil {
		t.Fatal(err)
	}
	return sys
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
	sys := r.nix.built
	want := []string{"build expanse", "profile " + sys, "switch " + sys}
	if !reflect.DeepEqual(r.nix.calls, want) {
		t.Errorf("calls = %v, want %v", r.nix.calls, want)
	}
	st, err := ReadStatus(r.u.StatusPath)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != StateDone || st.Version != "1.12.0" || st.From != "1.11.3" || st.System != sys {
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

// A configuration.nix that takes expanse from elsewhere builds the old release from the new source.
func TestApplyRefusesASystemThatDoesNotRunTheNewRelease(t *testing.T) {
	r := newApplyRig(t)
	r.nix.built = fakeSystem(t, "1.11.0")
	err := r.u.Apply(context.Background(), "1.12.0", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "expanse-1.11.0") {
		t.Fatalf("err = %v, want one naming expanse-1.11.0", err)
	}
	if v, _ := r.u.Tree.Version("expanse"); v != "1.11.3" {
		t.Errorf("source after a refused build is %q", v)
	}
	if !reflect.DeepEqual(r.nix.calls, []string{"build expanse"}) {
		t.Errorf("went on after the refused build: %v", r.nix.calls)
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
	if !strings.HasPrefix(r.nix.subst, "file://") {
		t.Fatalf("substituter = %q", r.nix.subst)
	}
	if r.nix.substInfo != nil {
		t.Errorf("the cache was not unpacked where the substituter points: %v", r.nix.substInfo)
	}
}

// Downloads are dropped once the node runs the release; other releases' downloads go before a fetch.
func TestApplyRemovesDownloadsItNoLongerNeeds(t *testing.T) {
	r := newApplyRig(t)
	cache := r.u.Source.CacheDir
	old := filepath.Join(cache, "1.11.3")
	os.MkdirAll(old, 0o750)
	os.WriteFile(filepath.Join(old, "expanse-1.11.3.tar.gz"), []byte("old"), 0o644)
	os.WriteFile(filepath.Join(cache, "notes"), []byte("not a release"), 0o644)

	r.nix.buildErr = errors.New("builder failed")
	if err := r.u.Apply(context.Background(), "1.12.0", io.Discard); err == nil {
		t.Fatal("a failed build was reported as success")
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("another release's downloads survived a fetch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cache, "1.12.0", "expanse-1.12.0.tar.gz")); err != nil {
		t.Errorf("a failed upgrade dropped its downloads: %v", err)
	}

	r.nix.buildErr = nil
	if err := r.u.Apply(context.Background(), "1.12.0", io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cache, "1.12.0")); !os.IsNotExist(err) {
		t.Errorf("downloads survived a finished upgrade: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cache, "notes")); err != nil {
		t.Errorf("a file that is no release's downloads was removed: %v", err)
	}
}
