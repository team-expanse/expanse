package volume

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func newMarks(t *testing.T) SplitBrainMarks {
	t.Helper()
	m, err := NewSplitBrainMarks(filepath.Join(t.TempDir(), "split-brain"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestNothingIsMarkedUntilTheHandlerRuns(t *testing.T) {
	m := newMarks(t)
	got, err := m.Marked("vol-a1")
	if err != nil || got {
		t.Fatalf("Marked = %v, %v; want false, nil", got, err)
	}
}

// The handler is what the kernel runs, so the test runs it the way drbdadm does: sh -c with the resource in the environment.
func runHandler(t *testing.T, m SplitBrainMarks, res string) {
	t.Helper()
	cmd := exec.Command("sh", "-c", m.Handler())
	cmd.Env = []string{"DRBD_RESOURCE=" + res}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("handler failed: %v: %s", err, out)
	}
}

func TestTheHandlerMarksOnlyTheResourceItWasRunFor(t *testing.T) {
	m := newMarks(t)
	runHandler(t, m, "vol-a1")
	if got, _ := m.Marked("vol-a1"); !got {
		t.Error("vol-a1 should be marked")
	}
	if got, _ := m.Marked("vol-b2"); got {
		t.Error("vol-b2 should not be marked")
	}
}

func TestTheHandlerNeedsNoToolsOutsideTheShell(t *testing.T) {
	m := newMarks(t)
	cmd := exec.Command("sh", "-c", m.Handler())
	cmd.Env = []string{"DRBD_RESOURCE=vol-a1", "PATH=/nonexistent"}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("handler failed without PATH: %v: %s", err, out)
	}
}

func TestMarkingTwiceIsHarmless(t *testing.T) {
	m := newMarks(t)
	runHandler(t, m, "vol-a1")
	runHandler(t, m, "vol-a1")
	if got, _ := m.Marked("vol-a1"); !got {
		t.Error("still expected to be marked")
	}
}

func TestClearForgetsAMarkAndToleratesAMissingOne(t *testing.T) {
	m := newMarks(t)
	runHandler(t, m, "vol-a1")
	if err := m.Clear("vol-a1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.Marked("vol-a1"); got {
		t.Error("mark survived Clear")
	}
	if err := m.Clear("vol-a1"); err != nil {
		t.Errorf("clearing nothing: %v", err)
	}
}

func TestMarksSurviveARestart(t *testing.T) {
	m := newMarks(t)
	runHandler(t, m, "vol-a1")
	again, err := NewSplitBrainMarks(m.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := again.Marked("vol-a1"); !got {
		t.Error("a new instance over the same directory lost the mark")
	}
}

func TestADirectoryTheShellCouldMisreadIsRefused(t *testing.T) {
	for _, dir := range []string{"", "relative/dir", "/var/lib/a b", "/var/lib/a;reboot", `/var/lib/"x"`, "/var/lib/$HOME", "/var/lib/a\nb"} {
		if _, err := NewSplitBrainMarks(dir); err == nil {
			t.Errorf("accepted %q", dir)
		}
	}
}

func TestNewSplitBrainMarksCreatesTheDirectory(t *testing.T) {
	m := newMarks(t)
	if st, err := os.Stat(m.Dir); err != nil || !st.IsDir() {
		t.Fatalf("directory not created: %v", err)
	}
}

func TestTheHandlerIsAShellBuiltinRedirectOnly(t *testing.T) {
	m := newMarks(t)
	if strings.ContainsAny(m.Handler(), "\"\\\r\n") {
		t.Errorf("handler %q would be rejected by the DRBD config renderer", m.Handler())
	}
}
