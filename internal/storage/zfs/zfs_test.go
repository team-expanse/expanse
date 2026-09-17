package zfs

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"strings"
	"testing"

	experrors "github.com/expanse/expanse/internal/errors"
)

// fakeExec builds an Exec whose runCmd emits the given stdout and stderr
// with the given exit status, recording every invocation.
type fakeExec struct {
	*Exec
	calls   [][]string
	stdout  string
	stderr  string
	exitErr int
}

func fakeRun(stdout, stderr string, exitErr int) func(e *fakeExec) func(context.Context, string, ...string) *exec.Cmd {
	return func(e *fakeExec) func(context.Context, string, ...string) *exec.Cmd {
		return func(ctx context.Context, name string, args ...string) *exec.Cmd {
			e.calls = append(e.calls, append([]string{name}, args...))
			script := buildShScript(stdout, stderr, exitErr)
			return exec.CommandContext(ctx, "sh", "-c", script)
		}
	}
}

func buildShScript(stdout, stderr string, exitErr int) string {
	// Emit fixtures through shell quoting-safe heredoc-free printf.
	var b strings.Builder
	b.WriteString("printf '%s' " + shQuote(stdout) + "\n")
	if stderr != "" {
		b.WriteString("printf '%s' " + shQuote(stderr) + " >&2\n")
	}
	b.WriteString("exit " + itoa(exitErr) + "\n")
	return b.String()
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

func newFake(stdout, stderr string, exitErr int) *fakeExec {
	f := &fakeExec{stdout: stdout, stderr: stderr, exitErr: exitErr}
	f.Exec = New()
	f.runCmd = fakeRun(stdout, stderr, exitErr)(f)
	return f
}

func ctx(t *testing.T) context.Context {
	return context.Background()
}

func TestCreateZvolDefaultsAndOverrides(t *testing.T) {
	f := newFake("", "", 0)
	err := f.CreateZvol(ctx(t), "tank/exvol/vol-1", 10<<30, map[string]string{"compression": "lz4"})
	if err != nil {
		t.Fatalf("CreateZvol: %v", err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(f.calls))
	}
	got := f.calls[0]
	if got[0] != "zfs" || got[1] != "create" {
		t.Fatalf("unexpected command %v", got)
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "-V 10737418240") {
		t.Errorf("size not passed as bytes: %v", got)
	}
	if !strings.Contains(joined, "-b 16k") {
		t.Errorf("default volblocksize missing: %v", got)
	}
	for _, want := range []string{"sync=always", "logbias=throughput", "primarycache=metadata"} {
		if !strings.Contains(joined, want) {
			t.Errorf("default prop %s missing: %v", want, got)
		}
	}
	// Storage-class override must win over the default.
	if !strings.Contains(joined, "compression=lz4") || strings.Contains(joined, "compression=zstd") {
		t.Errorf("override compression=lz4 not applied: %v", got)
	}
	// volblocksize is passed via -b, not duplicated as -o.
	if strings.Contains(joined, "-o volblocksize") {
		t.Errorf("volblocksize duplicated as -o: %v", got)
	}
}

func TestCreateZvolDefaultsOnly(t *testing.T) {
	f := newFake("", "", 0)
	if err := f.CreateZvol(ctx(t), "tank/exvol/vol-2", 10<<30, nil); err != nil {
		t.Fatalf("CreateZvol: %v", err)
	}
	joined := strings.Join(f.calls[0], " ")
	for _, want := range []string{
		"-b 16k", "compression=zstd", "sync=always",
		"logbias=throughput", "primarycache=metadata",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("default prop %s missing: %v", want, joined)
		}
	}
}

func TestDestroyZvolRecursive(t *testing.T) {
	f := newFake("", "", 0)
	if err := f.DestroyZvol(ctx(t), "tank/exvol/vol-1", true); err != nil {
		t.Fatalf("DestroyZvol: %v", err)
	}
	got := strings.Join(f.calls[0], " ")
	if !strings.Contains(got, "destroy -r -R tank/exvol/vol-1") {
		t.Errorf("recursive flags missing: %v", got)
	}

	f2 := newFake("", "", 0)
	if err := f2.DestroyZvol(ctx(t), "tank/exvol/vol-1", false); err != nil {
		t.Fatalf("DestroyZvol: %v", err)
	}
	if strings.Contains(strings.Join(f2.calls[0], " "), "-r") {
		t.Errorf("non-recursive destroy got -r: %v", f2.calls[0])
	}
}

func TestSnapshotAndDestroySnapshot(t *testing.T) {
	f := newFake("", "", 0)
	if err := f.Snapshot(ctx(t), "tank/exvol/vol-1", "resync-42"); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got := strings.Join(f.calls[0], " "); got != "zfs snapshot tank/exvol/vol-1@resync-42" {
		t.Errorf("snapshot command: %v", got)
	}
	if err := f.DestroySnapshot(ctx(t), "tank/exvol/vol-1", "resync-42"); err != nil {
		t.Fatalf("DestroySnapshot: %v", err)
	}
	if got := strings.Join(f.calls[1], " "); got != "zfs destroy tank/exvol/vol-1@resync-42" {
		t.Errorf("destroy-snapshot command: %v", got)
	}
}

func TestListSnapshots(t *testing.T) {
	out := "tank/exvol/vol-1@resync-10\n" +
		"tank/exvol/vol-1@resync-20\n" +
		"tank/exvol/vol-1@resync-30\n"
	f := newFake(out, "", 0)
	snaps, err := f.ListSnapshots(ctx(t), "tank/exvol/vol-1")
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	want := []string{"resync-10", "resync-20", "resync-30"}
	if len(snaps) != len(want) {
		t.Fatalf("got %v, want %v", snaps, want)
	}
	for i := range want {
		if snaps[i] != want[i] {
			t.Errorf("snap[%d] = %q, want %q", i, snaps[i], want[i])
		}
	}
	got := strings.Join(f.calls[0], " ")
	for _, wantFlag := range []string{"-H", "-p", "-d 1", "-t snapshot", "-o name"} {
		if !strings.Contains(got, wantFlag) {
			t.Errorf("parseable-flags missing %s: %v", wantFlag, got)
		}
	}
}

func TestListSnapshotsEmpty(t *testing.T) {
	f := newFake("", "", 0)
	snaps, err := f.ListSnapshots(ctx(t), "tank/exvol/vol-1")
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(snaps) != 0 {
		t.Errorf("expected no snapshots, got %v", snaps)
	}
}

func TestSendIncrementalAndFull(t *testing.T) {
	f := newFake("STREAMDATA", "", 0)
	var buf bytes.Buffer
	if err := f.Send(ctx(t), "tank/vol-1", "resync-10", "resync-20", &buf); err != nil {
		t.Fatalf("Send incremental: %v", err)
	}
	if got := strings.Join(f.calls[0], " "); got != "zfs send -i tank/vol-1@resync-10 tank/vol-1@resync-20" {
		t.Errorf("incremental send command: %v", got)
	}
	if buf.String() != "STREAMDATA" {
		t.Errorf("stdout not streamed to writer: %q", buf.String())
	}

	f2 := newFake("FULLSTREAM", "", 0)
	if err := f2.Send(ctx(t), "tank/vol-1", "", "resync-1", io.Discard); err != nil {
		t.Fatalf("Send full: %v", err)
	}
	if got := strings.Join(f2.calls[0], " "); got != "zfs send tank/vol-1@resync-1" {
		t.Errorf("full send must omit -i: %v", got)
	}
}

func TestReceive(t *testing.T) {
	f := newFake("", "", 0)
	stream := strings.NewReader("INCOMING")
	if err := f.Receive(ctx(t), "tank/exvol/vol-1", stream); err != nil {
		t.Fatalf("Receive: %v", err)
	}
	got := strings.Join(f.calls[0], " ")
	if !strings.Contains(got, "receive -F tank/exvol/vol-1") {
		t.Errorf("receive command: %v", got)
	}
}

func TestResize(t *testing.T) {
	f := newFake("", "", 0)
	if err := f.Resize(ctx(t), "tank/exvol/vol-1", 20<<30); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	got := strings.Join(f.calls[0], " ")
	if !strings.Contains(got, "volsize=21474836480") || !strings.Contains(got, "tank/exvol/vol-1") {
		t.Errorf("resize command: %v", got)
	}
}

func TestScrub(t *testing.T) {
	f := newFake("", "", 0)
	if err := f.Scrub(ctx(t), "tank"); err != nil {
		t.Fatalf("Scrub: %v", err)
	}
	if got := strings.Join(f.calls[0], " "); got != "zpool scrub tank" {
		t.Errorf("scrub command: %v", got)
	}
}

func TestPoolStatusParsing(t *testing.T) {
	healthy := "tank\tONLINE\t0\t0\t0\n" +
		"mirror-0\tONLINE\t0\t0\t0\n" +
		"nvme0n1p3\tONLINE\t0\t0\t0\n" +
		"nvme1n1p3\tONLINE\t0\t0\t0\n" +
		"errors: No known data errors\n"
	f := newFake(healthy, "", 0)
	ps, err := f.PoolStatus(ctx(t), "tank")
	if err != nil {
		t.Fatalf("PoolStatus: %v", err)
	}
	if ps.Health != "ONLINE" {
		t.Errorf("Health = %q, want ONLINE", ps.Health)
	}
	if len(ps.Devices) != 4 {
		t.Errorf("got %d devices, want 4: %+v", len(ps.Devices), ps.Devices)
	}
	if ps.Errors != 0 {
		t.Errorf("Errors = %d, want 0", ps.Errors)
	}
}

func TestPoolStatusDegraded(t *testing.T) {
	degraded := "tank\tDEGRADED\t0\t0\t0\n" +
		"mirror-0\tDEGRADED\t0\t0\t0\n" +
		"nvme0n1p3\tONLINE\t0\t0\t0\n" +
		"1662457128-5711\tUNAVAIL\t0\t0\t0\n" +
		"errors: No known data errors\n"
	f := newFake(degraded, "", 0)
	ps, err := f.PoolStatus(ctx(t), "tank")
	if err != nil {
		t.Fatalf("PoolStatus: %v", err)
	}
	if ps.Health != "DEGRADED" {
		t.Errorf("Health = %q, want DEGRADED", ps.Health)
	}
	if len(ps.Devices) != 4 {
		t.Errorf("got %d devices, want 4: %+v", len(ps.Devices), ps.Devices)
	}
	if ps.Devices[3].State != "UNAVAIL" {
		t.Errorf("failed vdev state = %q, want UNAVAIL", ps.Devices[3].State)
	}
}

func TestPoolStatusFaultedWithErrors(t *testing.T) {
	faulted := "tank\tFAULTED\t0\t0\t0\n" +
		"nvme0n1p3\tFAULTED\t0\t0\t0\n" +
		"errors: 12\n"
	f := newFake(faulted, "", 0)
	ps, err := f.PoolStatus(ctx(t), "tank")
	if err != nil {
		t.Fatalf("PoolStatus: %v", err)
	}
	if ps.Health != "FAULTED" {
		t.Errorf("Health = %q, want FAULTED", ps.Health)
	}
	if ps.Errors != 12 {
		t.Errorf("Errors = %d, want 12", ps.Errors)
	}
}

func TestPoolStatusMalformedIsTolerated(t *testing.T) {
	// Garbage rows are skipped, not a parse crash — real `zpool status`
	// output occasionally includes per-device scariness lines.
	f := newFake("something unexpected\nerrors: No known data errors\n", "", 0)
	ps, err := f.PoolStatus(ctx(t), "tank")
	if err != nil {
		t.Fatalf("PoolStatus: %v", err)
	}
	if ps.Health != "" {
		t.Errorf("Health = %q, want empty (no valid pool row)", ps.Health)
	}
	if len(ps.Devices) != 0 {
		t.Errorf("expected no devices from garbage, got %+v", ps.Devices)
	}
	if ps.Errors != 0 {
		t.Errorf("Errors = %d, want 0", ps.Errors)
	}
}

func TestNotFoundClassification(t *testing.T) {
	f := newFake("", "cannot open 'tank/nope': dataset does not exist\n", 1)
	_, err := f.ListSnapshots(ctx(t), "tank/nope")
	if experrors.KindOf(err) != experrors.KindNotFound {
		t.Errorf("KindOf = %v, want not_found (err: %v)", experrors.KindOf(err), err)
	}
}

func TestInternalClassification(t *testing.T) {
	f := newFake("", "internal error: bad stuff\n", 1)
	err := f.Scrub(ctx(t), "tank")
	if experrors.KindOf(err) != experrors.KindInternal {
		t.Errorf("KindOf = %v, want internal (err: %v)", experrors.KindOf(err), err)
	}
}

func TestCanceledContextIsTimeout(t *testing.T) {
	c, cancel := context.WithCancel(context.Background())
	cancel()
	f := newFake("", "", 0)
	err := f.Scrub(c, "tank")
	if experrors.KindOf(err) != experrors.KindTimeout {
		t.Errorf("KindOf = %v, want timeout (err: %v)", experrors.KindOf(err), err)
	}
}

// Compile-time check that the interface shape matches the spec exactly.
var _ ZFS = (*Exec)(nil)
