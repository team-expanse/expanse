package rollout

import (
	"strings"
	"testing"

	"github.com/expanse/expanse/internal/cluster/control"
	"github.com/expanse/expanse/internal/storage"
)

func report(nodes ...control.NodeStatus) *control.Report {
	return &control.Report{Leader: "10.0.0.1:7444", Nodes: nodes}
}

func TestClusterHealth(t *testing.T) {
	ok := control.NodeStatus{ID: "n1"}
	cases := []struct {
		name string
		rep  *control.Report
		vols map[string]storage.VolumeState
		why  string // "" = healthy
	}{
		{"all well", report(ok, control.NodeStatus{ID: "n2"}), map[string]storage.VolumeState{"db": storage.StateHealthy, "solo": storage.StateUnderReplicated}, ""},
		{"no leader", &control.Report{Nodes: []control.NodeStatus{ok}}, nil, "no leader"},
		{"node unreachable", report(ok, control.NodeStatus{ID: "n2", Lifecycle: "unreachable"}), nil, "n2"},
		{"node degraded", report(ok, control.NodeStatus{ID: "n2", Degraded: true}), nil, "n2"},
		{"volume resyncing", report(ok), map[string]storage.VolumeState{"db": storage.StateResyncing}, "db"},
	}
	for _, c := range cases {
		h := ClusterHealth(c.rep, c.vols)
		if c.why == "" {
			if !h.OK {
				t.Errorf("%s: unhealthy: %s", c.name, h.Why)
			}
			continue
		}
		if h.OK || !strings.Contains(h.Why, c.why) {
			t.Errorf("%s: health %+v, want a reason naming %q", c.name, h, c.why)
		}
	}
}
