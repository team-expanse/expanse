package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/expanse/expanse/internal/blocks/vm/vmstate"
)

func TestAStartResumesAKeptSaveOnlyIntoItsPlacement(t *testing.T) {
	dir := savedAs(t, vmstate.KeepToken("a1b2"))
	if !resumeOrDiscard(dir, resumeTokens(&spec{StateToken: "r5", Placement: "a1b2"})...) {
		t.Fatal("the save kept for this placement was not resumed")
	}
	dir = savedAs(t, vmstate.KeepToken("a1b2"))
	if resumeOrDiscard(dir, resumeTokens(&spec{Placement: "c3d4"})...) {
		t.Error("a later placement resumed the save")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("a later placement left the stale save")
	}
}

func TestResumeTokensSkipAMissingPlacement(t *testing.T) {
	if got := resumeTokens(&spec{StateToken: "r5"}); len(got) != 1 || got[0] != "r5" {
		t.Errorf("tokens %q, want just r5", got)
	}
}

func TestAStopKeepsTheGuestWhenAskedOrAtShutdown(t *testing.T) {
	keepDir := t.TempDir()
	placed := &spec{Type: "vm/instance", Placement: "a1b2"}
	marker := vmstate.KeepPath(keepDir, "default-vm-0")
	cases := []struct {
		name     string
		s        *spec
		marker   bool
		stopping bool
		want     string
	}{
		{"a plain stop", placed, false, false, ""},
		{"an update", placed, true, false, "an update"},
		{"a node shutdown", placed, false, true, "the node's shutdown"},
		{"no placement id", &spec{Type: "vm/instance"}, true, true, ""},
	}
	for _, c := range cases {
		if c.marker {
			if err := os.WriteFile(marker, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		got := keepReason("default-vm-0", c.s, keepDir, func() bool { return c.stopping })
		if got != c.want {
			t.Errorf("%s: reason %q, want %q", c.name, got, c.want)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Errorf("%s: the keep request outlived the stop", c.name)
			_ = os.Remove(filepath.Clean(marker))
		}
	}
}

func TestSystemStoppingReadsSystemdsState(t *testing.T) {
	for out, want := range map[string]bool{"stopping\n": true, "running\n": false, "degraded\n": false} {
		if got := systemStoppingFrom(out); got != want {
			t.Errorf("%q: %v, want %v", out, got, want)
		}
	}
}
