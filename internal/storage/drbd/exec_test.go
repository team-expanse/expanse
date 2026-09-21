package drbd

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

func TestCommandArguments(t *testing.T) {
	cases := []struct {
		name string
		call func(*Exec) error
		want []string
	}{
		{
			"create-md", func(e *Exec) error { return e.CreateMD(context.Background(), "r0", 7) },
			[]string{"drbdadm", "create-md", "--force", "--max-peers=7", "r0"},
		},
		{"up", func(e *Exec) error { return e.Up(context.Background(), "r0") }, []string{"drbdadm", "up", "r0"}},
		{"down", func(e *Exec) error { return e.Down(context.Background(), "r0") }, []string{"drbdadm", "down", "r0"}},
		{
			"primary", func(e *Exec) error { return e.Primary(context.Background(), "r0") },
			[]string{"drbdadm", "primary", "r0"},
		},
		{
			"force-primary", func(e *Exec) error { return e.ForcePrimary(context.Background(), "r0") },
			[]string{"drbdadm", "primary", "--force", "r0"},
		},
		{
			"secondary", func(e *Exec) error { return e.Secondary(context.Background(), "r0") },
			[]string{"drbdadm", "secondary", "r0"},
		},
		{"adjust", func(e *Exec) error { return e.Adjust(context.Background(), "r0") }, []string{"drbdadm", "adjust", "r0"}},
		{"resize", func(e *Exec) error { return e.Resize(context.Background(), "r0") }, []string{"drbdadm", "resize", "r0"}},
		{"verify", func(e *Exec) error { return e.Verify(context.Background(), "r0") }, []string{"drbdadm", "verify", "r0"}},
		{
			"invalidate", func(e *Exec) error { return e.Invalidate(context.Background(), "r0") },
			[]string{"drbdadm", "invalidate", "r0"},
		},
		{"connect", func(e *Exec) error { return e.Connect(context.Background(), "r0") }, []string{"drbdadm", "connect", "r0"}},
		{
			"connect-discarding", func(e *Exec) error { return e.ConnectDiscarding(context.Background(), "r0") },
			[]string{"drbdadm", "connect", "--discard-my-data", "r0"},
		},
		{
			"disconnect", func(e *Exec) error { return e.Disconnect(context.Background(), "r0") },
			[]string{"drbdadm", "disconnect", "r0"},
		},
		{
			"forget-peer", func(e *Exec) error { return e.ForgetPeer(context.Background(), "r0", 2) },
			[]string{"drbdsetup", "forget-peer", "r0", "2"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake("", "", 0)
			if err := tc.call(f.Exec); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if len(f.calls) != 1 || !reflect.DeepEqual(f.calls[0], tc.want) {
				t.Fatalf("calls = %v, want [%v]", f.calls, tc.want)
			}
		})
	}
}

func TestErrorsNameTheResourceAndCarryStderr(t *testing.T) {
	f := newFake("", "r7: Failure: (162) Invalid configuration request", 1)
	err := f.Adjust(context.Background(), "r7")
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"r7", "(162) Invalid configuration request", "adjust"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

func TestUnknownResourceIsNotFound(t *testing.T) {
	f := newFake("", "'nosuch' not defined in your config (for this host).", 10)
	err := f.Up(context.Background(), "nosuch")
	if experrors.KindOf(err) != experrors.KindNotFound {
		t.Fatalf("kind = %q, want not_found (%v)", experrors.KindOf(err), err)
	}
}

func TestResourceNamesCannotInjectOptions(t *testing.T) {
	for _, bad := range []string{"", "-r0", "r 0", "r0;rm", "../r0"} {
		f := newFake("", "", 0)
		err := f.Up(context.Background(), bad)
		if experrors.KindOf(err) != experrors.KindInvalid {
			t.Errorf("Up(%q) kind = %q, want invalid", bad, experrors.KindOf(err))
		}
		if len(f.calls) != 0 {
			t.Errorf("Up(%q) reached exec: %v", bad, f.calls)
		}
	}
}

func TestCreateMDRejectsBadMaxPeers(t *testing.T) {
	for _, n := range []int{0, -1, 32} {
		f := newFake("", "", 0)
		if err := f.CreateMD(context.Background(), "r0", n); experrors.KindOf(err) != experrors.KindInvalid {
			t.Errorf("CreateMD(max-peers=%d) kind = %q, want invalid", n, experrors.KindOf(err))
		}
	}
}

func TestCanceledContextIsATimeout(t *testing.T) {
	f := newFake("", "", 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.Up(ctx, "r0"); experrors.KindOf(err) != experrors.KindTimeout {
		t.Fatalf("kind = %q, want timeout (%v)", experrors.KindOf(err), err)
	}
}

func TestHasMetadata(t *testing.T) {
	cases := []struct {
		name           string
		stdout, stderr string
		exit           int
		want           bool
		wantErr        bool
	}{
		{"present", "# DRBD meta data dump\n", "", 0, true, false},
		{"blank, reported on stderr", "", "No valid meta data found\n", 255, false, false},
		{"blank, reported on stdout", "No valid meta data found\n", "", 255, false, false},
		{"unclean metadata is still metadata", "", "Found meta data is \"unclean\", please apply-al first\n", 255, true, false},
		{"anything else is an error, never a green light to overwrite", "", "drbdmeta: cannot open /dev/vdb\n", 255, false, true},
		{"silent failure is an error", "", "", 255, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(tc.stdout, tc.stderr, tc.exit)
			got, err := f.HasMetadata(context.Background(), "r0")
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Errorf("got %v, %v; want %v, err=%v", got, err, tc.want, tc.wantErr)
			}
			if want := []string{"drbdadm", "dump-md", "r0"}; !reflect.DeepEqual(f.calls[0], want) {
				t.Errorf("ran %v, want %v", f.calls[0], want)
			}
		})
	}
}

func TestAdjustPending(t *testing.T) {
	f := newFake("drbdsetup del-peer r0 1\n", "", 0)
	pending, err := f.AdjustPending(context.Background(), "r0")
	if err != nil || !pending {
		t.Errorf("drift not reported: %v, %v", pending, err)
	}
	if want := []string{"drbdadm", "-d", "adjust", "r0"}; !reflect.DeepEqual(f.calls[0], want) {
		t.Errorf("ran %v, want %v", f.calls[0], want)
	}
	for _, quiet := range []string{"", "\n", "  \n"} {
		if pending, err := newFake(quiet, "", 0).AdjustPending(context.Background(), "r0"); err != nil || pending {
			t.Errorf("in-sync output %q reported as drift: %v, %v", quiet, pending, err)
		}
	}
	if _, err := newFake("", "r0: Failure: (162)", 1).AdjustPending(context.Background(), "r0"); err == nil {
		t.Error("a failing dry run must be an error, not 'no drift'")
	}
}

func TestFailureTextOnStdoutIsStillClassified(t *testing.T) {
	err := newFake("r0: No such resource\n", "", 10).Down(context.Background(), "r0")
	if experrors.KindOf(err) != experrors.KindNotFound {
		t.Errorf("kind = %q, want not_found (%v)", experrors.KindOf(err), err)
	}
}
