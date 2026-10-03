package web

import (
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/expanse/expanse/internal/cluster/control"
	"github.com/expanse/expanse/internal/cluster/nodelc"
	"github.com/expanse/expanse/internal/storage"
	pb "github.com/expanse/expanse/proto"
)

func TestPhasePillMapsEveryPhaseToALabelAndTone(t *testing.T) {
	cases := map[pb.Phase]pill{
		pb.Phase_RUNNING:           {Label: "Running", Tone: "ok"},
		pb.Phase_PENDING:           {Label: "Pending", Tone: "info"},
		pb.Phase_SCHEDULING:        {Label: "Scheduling", Tone: "info"},
		pb.Phase_PROVISIONING:      {Label: "Provisioning", Tone: "info"},
		pb.Phase_STARTING:          {Label: "Starting", Tone: "info"},
		pb.Phase_UPDATING:          {Label: "Updating", Tone: "info"},
		pb.Phase_DRAINING:          {Label: "Draining", Tone: "warn"},
		pb.Phase_DEGRADED:          {Label: "Degraded", Tone: "warn"},
		pb.Phase_TERMINATING:       {Label: "Terminating", Tone: "warn"},
		pb.Phase_FAILED:            {Label: "Failed", Tone: "crit"},
		pb.Phase_LOST:              {Label: "Lost", Tone: "crit"},
		pb.Phase_TERMINATED:        {Label: "Terminated", Tone: "neutral"},
		pb.Phase_PHASE_UNSPECIFIED: {Label: "Unknown", Tone: "neutral"},
	}
	for in, want := range cases {
		if got := phasePill(in); got != want {
			t.Errorf("phasePill(%v) = %+v, want %+v", in, got, want)
		}
	}
	// Every enum value must be covered; a new phase must not fall through
	// to the raw wire constant.
	for name, v := range pb.Phase_value {
		if got := phasePill(pb.Phase(v)); got.Label == "" || got.Label == name {
			t.Errorf("phasePill(%s) not humanized: %+v", name, got)
		}
	}
}

func TestVolumePillMapsEveryState(t *testing.T) {
	cases := map[storage.VolumeState]pill{
		storage.StateHealthy:             {Label: "Healthy", Tone: "ok"},
		storage.StateCreating:            {Label: "Creating", Tone: "info"},
		storage.StateResyncing:           {Label: "Resyncing", Tone: "info"},
		storage.StateDeleting:            {Label: "Deleting", Tone: "info"},
		storage.StateDegraded:            {Label: "Degraded", Tone: "warn"},
		storage.StateUnderReplicated:     {Label: "Under-replicated", Tone: "warn"},
		storage.StateReadOnly:            {Label: "Read-only", Tone: "warn"},
		storage.StateFailed:              {Label: "Failed", Tone: "crit"},
		storage.StateNeedsManualRecovery: {Label: "Needs manual recovery", Tone: "crit"},
		storage.VolumeState(""):          {Label: "Unknown", Tone: "neutral"},
	}
	for in, want := range cases {
		if got := volumePill(in); got != want {
			t.Errorf("volumePill(%q) = %+v, want %+v", in, got, want)
		}
	}
}

func TestHealthPill(t *testing.T) {
	cases := map[pb.Health]pill{
		pb.Health_HEALTH_HEALTHY:     {Label: "Healthy", Tone: "ok"},
		pb.Health_HEALTH_DEGRADED:    {Label: "Degraded", Tone: "warn"},
		pb.Health_HEALTH_UNHEALTHY:   {Label: "Unhealthy", Tone: "crit"},
		pb.Health_HEALTH_UNKNOWN:     {Label: "Unknown", Tone: "neutral"},
		pb.Health_HEALTH_UNSPECIFIED: {Label: "Unknown", Tone: "neutral"},
	}
	for in, want := range cases {
		if got := healthPill(in); got != want {
			t.Errorf("healthPill(%v) = %+v, want %+v", in, got, want)
		}
	}
}

func TestNodePillPrefersTheWorstCondition(t *testing.T) {
	cases := []struct {
		in   control.NodeStatus
		want pill
	}{
		{control.NodeStatus{State: "leader"}, pill{Label: "Online", Tone: "ok"}},
		{control.NodeStatus{State: "voter/cordoned"}, pill{Label: "Cordoned", Tone: "neutral"}},
		{control.NodeStatus{State: "voter/draining"}, pill{Label: "Draining", Tone: "warn"}},
		{control.NodeStatus{State: "voter", Degraded: true}, pill{Label: "Degraded", Tone: "warn"}},
		{control.NodeStatus{Lifecycle: nodelc.StateUnreachable}, pill{Label: "Unreachable", Tone: "warn"}},
		{control.NodeStatus{Lifecycle: nodelc.StateFailed, Degraded: true}, pill{Label: "Failed", Tone: "crit"}},
	}
	for _, c := range cases {
		if got := nodePill(c.in); got != c.want {
			t.Errorf("nodePill(%+v) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

func TestNodeHealthPillParsesTheStatusValue(t *testing.T) {
	cases := map[string]pill{
		"health=healthy":  {Label: "Healthy", Tone: "ok"},
		"health=degraded": {Label: "Degraded", Tone: "warn"},
		"health=unhealthy degraded=true writable=false": {Label: "Unhealthy", Tone: "crit"},
		"": {Label: "Unknown", Tone: "neutral"},
	}
	for in, want := range cases {
		if got := nodeHealthPill(in); got != want {
			t.Errorf("nodeHealthPill(%q) = %+v, want %+v", in, got, want)
		}
	}
}

func TestAgentPill(t *testing.T) {
	cases := map[pb.NodeStatus_Status]pill{
		pb.NodeStatus_STATUS_IDLE:        {Label: "Idle", Tone: "ok"},
		pb.NodeStatus_STATUS_RECONCILING: {Label: "Reconciling", Tone: "info"},
		pb.NodeStatus_STATUS_DEGRADED:    {Label: "Degraded", Tone: "warn"},
		pb.NodeStatus_STATUS_ERROR:       {Label: "Error", Tone: "crit"},
		pb.NodeStatus_STATUS_UNSPECIFIED: {Label: "Unknown", Tone: "neutral"},
	}
	for in, want := range cases {
		if got := agentPill(in); got != want {
			t.Errorf("agentPill(%v) = %+v, want %+v", in, got, want)
		}
	}
}

func TestRoleLabel(t *testing.T) {
	cases := map[string]string{"voter": "Voter", "witness": "Witness", "nonvoter": "Non-voter", "": "Voter"}
	for in, want := range cases {
		if got := roleLabel(in); got != want {
			t.Errorf("roleLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHumanBytesRoundsMeasuredSizes(t *testing.T) {
	cases := map[any]string{
		int64(0):                  "0 B",
		int64(512):                "512 B",
		int64(2 << 40):            "2 TiB",
		uint64(960197124096):      "894.3 GiB",
		uint64(812_000_000 << 10): "774.4 GiB",
		int(1536):                 "1.5 KiB",
	}
	for in, want := range cases {
		if got := humanBytesAny(in); got != want {
			t.Errorf("humanBytesAny(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestHeartbeatPillIsStaleForAnUnreachableNode(t *testing.T) {
	up := control.NodeStatus{ID: "n1"}
	down := control.NodeStatus{ID: "n3", Lifecycle: nodelc.StateUnreachable}
	failed := control.NodeStatus{ID: "n4", Lifecycle: nodelc.StateFailed}
	if got := heartbeatPill(up, "health=healthy"); got != (pill{Label: "Healthy", Tone: "ok"}) {
		t.Errorf("reachable node: %+v", got)
	}
	for _, n := range []control.NodeStatus{down, failed} {
		if got := heartbeatPill(n, "health=healthy"); got != pillStale {
			t.Errorf("%s: heartbeatPill = %+v, want %+v", n.ID, got, pillStale)
		}
	}
}

func TestErrTextShowsTheStatusMessageOnly(t *testing.T) {
	cases := map[error]string{
		status.Error(codes.InvalidArgument, "spec.replicas: must be >= 0"): "spec.replicas: must be >= 0",
		errors.New("plain failure"):                                        "plain failure",
	}
	for err, want := range cases {
		if got := errText(err); got != want {
			t.Errorf("errText(%v) = %q, want %q", err, got, want)
		}
	}
}
