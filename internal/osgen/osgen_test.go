package osgen

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/expanse/expanse/internal/cluster/join"
)

func TestToRemoveKeepsTheNewestAndNeverTheRunningOrBooted(t *testing.T) {
	gens := []Generation{{Number: 7}, {Number: 6}, {Number: 5, Booted: true}, {Number: 4}, {Number: 3, Current: true}, {Number: 2}}
	if got, want := ToRemove(gens, 2), []int{4, 2}; !reflect.DeepEqual(got, want) {
		t.Errorf("ToRemove(keep 2) = %v, want %v", got, want)
	}
	if got := ToRemove(gens, 10); len(got) != 0 {
		t.Errorf("ToRemove(keep 10) = %v, want nothing", got)
	}
	if got, want := ToRemove(gens, 0), []int{6, 4, 2}; !reflect.DeepEqual(got, want) {
		t.Errorf("ToRemove(keep 0) = %v, want %v: keep is at least 1, and the newest stays", got, want)
	}
}

// fakeSystem makes a system closure whose sw/bin/expanse resolves into an expanse-<version> package.
func fakeSystem(t *testing.T, version string) string {
	t.Helper()
	root := t.TempDir()
	pkg := filepath.Join(root, "abc123-expanse-"+version, "bin")
	sys := filepath.Join(root, "def456-nixos-system", "sw", "bin")
	for _, d := range []string{pkg, sys} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(pkg, "expanse"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(pkg, "expanse"), filepath.Join(sys, "expanse")); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, "def456-nixos-system")
}

func TestReleaseOfReadsTheSystemsExpanse(t *testing.T) {
	if got := ReleaseOf(fakeSystem(t, "1.14.0")); got != "1.14.0" {
		t.Errorf("ReleaseOf = %q, want 1.14.0", got)
	}
	if got := ReleaseOf(t.TempDir()); got != "" {
		t.Errorf("ReleaseOf(no expanse) = %q, want empty", got)
	}
}

func TestReleaseSystemsRecordAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "release-systems.json")
	if got := LoadReleaseSystems(path); len(got) != 0 {
		t.Errorf("a missing file loads %v, want empty", got)
	}
	for sys, v := range map[string]string{"/nix/store/a-sys": "1.13.1", "/nix/store/b-sys": "1.14.0"} {
		if err := RecordReleaseSystem(path, sys, v); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]string{"/nix/store/a-sys": "1.13.1", "/nix/store/b-sys": "1.14.0"}
	if got := LoadReleaseSystems(path); !reflect.DeepEqual(got, want) {
		t.Errorf("LoadReleaseSystems = %v, want %v", got, want)
	}
}

func TestDriftNamesARunningSystemNoReleaseBuilt(t *testing.T) {
	for _, c := range []struct {
		gens []Generation
		want string
	}{
		{[]Generation{{Number: 2, Current: true, Release: "1.14.0", Exact: true}}, ""},
		{[]Generation{{Number: 2, Current: true, Release: "1.14.0", Recorded: true}}, "1.14.0, modified"},
		{[]Generation{{Number: 2, Current: true, Release: "1.14.0"}}, ""},
		{[]Generation{{Number: 2, Release: "1.14.0", Exact: true}}, ""},
	} {
		if got := Drift(c.gens); got != c.want {
			t.Errorf("Drift(%+v) = %q, want %q", c.gens, got, c.want)
		}
	}
}

// Everything under /nodes/ that parses as JSON is read as a node record (control.Status, join, nodelc).
func TestKeysStayOutOfTheNodeRecords(t *testing.T) {
	for _, k := range []string{string(ReportKey("n1")), string(RequestKey("n1"))} {
		if strings.HasPrefix(k, join.NodesKeyPrefix) {
			t.Errorf("%s is under %s, where it would read as a phantom node", k, join.NodesKeyPrefix)
		}
	}
}
