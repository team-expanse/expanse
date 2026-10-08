package lvm

import (
	"context"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"testing"

	experrors "github.com/expanse/expanse/internal/errors"
)

// fakeExec answers every command with canned output and records the calls.
type fakeExec struct {
	*Exec
	calls [][]string
}

func newFake(stdout, stderr string, exit int) *fakeExec {
	f := &fakeExec{Exec: New()}
	f.runCmd = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		f.calls = append(f.calls, append([]string{name}, args...))
		script := "printf '%s' \"$1\"; printf '%s' \"$2\" >&2; exit $3"
		return exec.CommandContext(ctx, "sh", "-c", script, "sh", stdout, stderr, strconv.Itoa(exit))
	}
	return f
}

var ctx = context.Background()

func TestCommandArguments(t *testing.T) {
	const mib = 1 << 20
	cases := []struct {
		name string
		call func(*Exec) error
		want []string
	}{
		{
			"create-vg", func(e *Exec) error { return e.CreateVG(ctx, "vg0", "/dev/vdb", "/dev/vdc") },
			[]string{"lvm", "vgcreate", "vg0", "/dev/vdb", "/dev/vdc"},
		},
		{
			"extend-vg", func(e *Exec) error { return e.ExtendVG(ctx, "vg0", "/dev/vdd") },
			[]string{"lvm", "vgextend", "vg0", "/dev/vdd"},
		},
		{
			"thin-pool", func(e *Exec) error { return e.CreateThinPool(ctx, "vg0", "pool", 512*mib) },
			[]string{"lvm", "lvcreate", "--yes", "--type", "thin-pool", "--size", "536870912b", "--name", "pool", "vg0"},
		},
		{
			"thin", func(e *Exec) error { return e.CreateThin(ctx, "vg0", "pool", "v1", 64*mib) },
			[]string{"lvm", "lvcreate", "--yes", "--type", "thin", "--thinpool", "vg0/pool", "--virtualsize", "67108864b", "--name", "v1"},
		},
		{
			"thick", func(e *Exec) error { return e.CreateThick(ctx, "vg0", "v2", 64*mib) },
			[]string{"lvm", "lvcreate", "--yes", "--size", "67108864b", "--name", "v2", "vg0"},
		},
		{
			"extend", func(e *Exec) error { return e.Extend(ctx, "vg0", "v1", 128*mib) },
			[]string{"lvm", "lvextend", "--size", "134217728b", "vg0/v1"},
		},
		{
			"remove", func(e *Exec) error { return e.Remove(ctx, "vg0", "v1") },
			[]string{"lvm", "lvremove", "--force", "vg0/v1"},
		},
		{
			"snapshot", func(e *Exec) error { return e.Snapshot(ctx, "vg0", "v1", "v1-s1") },
			[]string{"lvm", "lvcreate", "--snapshot", "--name", "v1-s1", "vg0/v1"},
		},
		{
			"activate", func(e *Exec) error { return e.Activate(ctx, "vg0", "v1-s1") },
			[]string{"lvm", "lvchange", "--activate", "y", "--ignoreactivationskip", "vg0/v1-s1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake("", "", 0)
			if err := tc.call(f.Exec); err != nil {
				t.Fatal(err)
			}
			if len(f.calls) != 1 || !reflect.DeepEqual(f.calls[0], tc.want) {
				t.Errorf("ran %v, want [%v]", f.calls, tc.want)
			}
		})
	}
}

func TestReportsAskForParseableOutput(t *testing.T) {
	f := newFake("  vg0|100|50\n", "", 0)
	if _, err := f.VG(ctx, "vg0"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"lvm", "vgs", "--noheadings", "--units", "b", "--nosuffix", "--separator", "|",
		"--options", "vg_name,vg_size,vg_free", "vg0",
	}
	if !reflect.DeepEqual(f.calls[0], want) {
		t.Errorf("ran %v, want %v", f.calls[0], want)
	}

	f = newFake("", "", 0)
	if _, err := f.List(ctx, "vg0"); err != nil {
		t.Fatal(err)
	}
	want = []string{
		"lvm", "lvs", "--noheadings", "--units", "b", "--nosuffix", "--separator", "|",
		"--options", "lv_name,vg_name,lv_size,lv_attr,pool_lv,origin,data_percent,metadata_percent", "vg0",
	}
	if !reflect.DeepEqual(f.calls[0], want) {
		t.Errorf("ran %v, want %v", f.calls[0], want)
	}
}

func TestGetSelectsOneVolume(t *testing.T) {
	f := newFake("  v1|vg0|1048576|-wi-a-----||||\n", "", 0)
	lv, err := f.Get(ctx, "vg0", "v1")
	if err != nil || lv.Name != "v1" {
		t.Fatalf("got %+v, %v", lv, err)
	}
	if last := f.calls[0][len(f.calls[0])-1]; last != "vg0/v1" {
		t.Errorf("selected %q, want vg0/v1", last)
	}
}

func TestGetRejectsAnUnexpectedRowCount(t *testing.T) {
	f := newFake("  a|vg0|1|-wi-a-----||||\n  b|vg0|1|-wi-a-----||||\n", "", 0)
	if _, err := f.Get(ctx, "vg0", "a"); experrors.KindOf(err) != experrors.KindInternal {
		t.Errorf("want KindInternal, got %v", err)
	}
}

// Find looks a volume up by name in every group: a block process does not know the node's group.
func TestFindSearchesEveryGroup(t *testing.T) {
	f := newFake("  vol-a1|vg9|1048576|Vwi-a-tz--|pool||1.00|\n", "", 0)
	lv, err := f.Find(ctx, "vol-a1")
	if err != nil || lv.VG != "vg9" {
		t.Fatalf("got %+v, %v", lv, err)
	}
	want := []string{
		"lvm", "lvs", "--noheadings", "--units", "b", "--nosuffix", "--separator", "|",
		"--options", "lv_name,vg_name,lv_size,lv_attr,pool_lv,origin,data_percent,metadata_percent",
		"--select", "lv_name=vol-a1",
	}
	if !reflect.DeepEqual(f.calls[0], want) {
		t.Errorf("ran %v, want %v", f.calls[0], want)
	}
	if _, err := newFake("", "", 0).Find(ctx, "vol-a1"); experrors.KindOf(err) != experrors.KindNotFound {
		t.Errorf("no match: want KindNotFound, got %v", err)
	}
	if _, err := newFake("", "", 0).Find(ctx, "a b"); experrors.KindOf(err) != experrors.KindInvalid {
		t.Errorf("bad name: want KindInvalid, got %v", err)
	}
}

func TestDevicePath(t *testing.T) {
	if got := DevicePath("vg0", "vol-a1"); got != "/dev/vg0/vol-a1" {
		t.Errorf("got %q", got)
	}
}

func TestErrorClassification(t *testing.T) {
	cases := []struct {
		stderr string
		want   experrors.Kind
	}{
		{"  Volume group \"vg9\" not found\n  Cannot process volume group vg9\n", experrors.KindNotFound},
		{"  Failed to find logical volume \"vg0/nope\"\n", experrors.KindNotFound},
		{"  Logical Volume \"v1\" already exists in volume group \"vg0\"\n", experrors.KindConflict},
		{"  Volume group \"vg0\" has insufficient free space (10 extents): 100 required.\n", experrors.KindResourceExhausted},
		{"  New size given (32 extents) not larger than existing size (96 extents)\n", experrors.KindInvalid},
		{"  something else broke\n", experrors.KindInternal},
	}
	for _, tc := range cases {
		f := newFake("", tc.stderr, 5)
		err := f.Remove(ctx, "vg0", "v1")
		if experrors.KindOf(err) != tc.want {
			t.Errorf("stderr %q: want %v, got %v", tc.stderr, tc.want, err)
		}
	}
}

func TestErrorCarriesStderr(t *testing.T) {
	err := newFake("", "  device /dev/vdb excluded by a filter\n", 5).CreateVG(ctx, "vg0", "/dev/vdb")
	if err == nil || !strings.Contains(err.Error(), "excluded by a filter") {
		t.Errorf("stderr lost: %v", err)
	}
}

func TestExtendToTheCurrentSizeIsNotAnError(t *testing.T) {
	stderr := "  No size change.\n"
	if err := newFake("", stderr, 5).Extend(ctx, "vg0", "v1", 64<<20); err != nil {
		t.Errorf("want nil, got %v", err)
	}
}

func TestCanceledContextIsATimeout(t *testing.T) {
	c, cancel := context.WithCancel(ctx)
	cancel()
	err := newFake("", "", 0).Remove(c, "vg0", "v1")
	if experrors.KindOf(err) != experrors.KindTimeout {
		t.Errorf("want KindTimeout, got %v", err)
	}
}

func TestInvalidInputNeverReachesLVM(t *testing.T) {
	cases := map[string]func(*Exec) error{
		"empty vg":          func(e *Exec) error { return e.Remove(ctx, "", "v") },
		"option-like vg":    func(e *Exec) error { return e.Remove(ctx, "-vg", "v") },
		"dot lv":            func(e *Exec) error { return e.Remove(ctx, "vg0", ".") },
		"dotdot lv":         func(e *Exec) error { return e.Remove(ctx, "vg0", "..") },
		"slash in lv":       func(e *Exec) error { return e.Remove(ctx, "vg0", "a/b") },
		"space in lv":       func(e *Exec) error { return e.Remove(ctx, "vg0", "a b") },
		"comma in lv":       func(e *Exec) error { _, err := e.Get(ctx, "vg0", "a,b"); return err },
		"zero size thick":   func(e *Exec) error { return e.CreateThick(ctx, "vg0", "v", 0) },
		"zero size pool":    func(e *Exec) error { return e.CreateThinPool(ctx, "vg0", "p", 0) },
		"zero size thin":    func(e *Exec) error { return e.CreateThin(ctx, "vg0", "p", "v", 0) },
		"zero extend":       func(e *Exec) error { return e.Extend(ctx, "vg0", "v", 0) },
		"bad pool":          func(e *Exec) error { return e.CreateThin(ctx, "vg0", "-p", "v", 1<<20) },
		"bad snapshot name": func(e *Exec) error { return e.Snapshot(ctx, "vg0", "v", "-s") },
		"relative pv":       func(e *Exec) error { return e.CreateVG(ctx, "vg0", "vdb") },
		"option-like pv":    func(e *Exec) error { return e.CreateVG(ctx, "vg0", "--force") },
		"no pvs":            func(e *Exec) error { return e.CreateVG(ctx, "vg0") },
		"no pvs to extend":  func(e *Exec) error { return e.ExtendVG(ctx, "vg0") },
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFake("", "", 0)
			err := call(f.Exec)
			if experrors.KindOf(err) != experrors.KindInvalid || len(f.calls) != 0 {
				t.Errorf("want KindInvalid and no command, got %v, calls %v", err, f.calls)
			}
		})
	}
}
