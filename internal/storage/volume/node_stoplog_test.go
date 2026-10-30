package volume

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// Stepping down unmounts the volume, so the node must say why it stopped leading.
func TestStoppingLeadershipLogsWhy(t *testing.T) {
	r := newNodeRig(t)
	var buf bytes.Buffer
	r.node.Log = slog.New(slog.NewTextHandler(&buf, nil))
	r.place(t, "n1", "n1", "n2")
	r.mustSync(t)
	waitStarted(t, r.lead)
	r.setPrimary(t, "n2")
	r.mustSync(t)
	out := buf.String()
	if !strings.Contains(out, "stepping down") || !strings.Contains(out, "primary is n2") {
		t.Errorf("log = %q, want the step-down and its reason", out)
	}
}
