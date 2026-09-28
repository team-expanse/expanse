package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/expanse/expanse/internal/version"
)

// The CLI and the web UI footer must report the one version the package build stamps.
func TestVersionCommandReportsTheStampedVersion(t *testing.T) {
	old := version.Version
	version.Version = "9.9.9"
	t.Cleanup(func() { version.Version = old })

	for _, args := range [][]string{nil, {"--json"}} {
		cmd := newVersionCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("version %v: %v", args, err)
		}
		if !strings.Contains(out.String(), "9.9.9") {
			t.Errorf("version %v printed %q, want the stamped 9.9.9", args, out.String())
		}
	}
}
