// Round-trip and enum coverage for proto/block.proto (Phase 04 T01).
//
// The canonical internal representation is protobuf (PHASE04.md §3.1); the
// generated Block message must survive marshal/unmarshal with every nested
// message populated — later cards (validation, scheduler, controller) rely
// on every field round-tripping exactly.
package proto

import (
	"testing"

	pbproto "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// fullBlock returns a Block with every nested message populated, mirroring
// the §3.1 canonical YAML example.
func fullBlock(t *testing.T) *Block {
	t.Helper()
	cfg, err := structpb.NewStruct(map[string]any{
		"serverName": "example.com",
		"root":       "/var/www",
		"port":       8080,
		"tls":        true,
		"aliases":    []any{"a.example.com", "b.example.com"},
		"limits": map[string]any{
			"body": "1M",
		},
	})
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return &Block{
		Metadata: &Metadata{
			Name:        "web",
			Namespace:   "default",
			Labels:      map[string]string{"app": "web", "tier": "front"},
			Annotations: map[string]string{"owner": "sam@corp"},
		},
		Spec: &BlockSpec{
			Type:     "web/nginx",
			Version:  "1.27",
			Replicas: ptr[int32](3),
			Strategy: &Strategy{
				Kind: StrategyKind_ACTIVE_ACTIVE,
				Update: &UpdateStrategy{
					Mode:                   UpdateMode_UPDATE_MODE_ROLLING,
					MaxUnavailable:         1,
					MaxSurge:               0,
					MinReadySeconds:        10,
					AutoRollback:           true,
					ConnectionDrainSeconds: 15,
				},
			},
			Runtime: Runtime_RUNTIME_SYSTEMD,
			Resources: &Resources{
				Requests: &ResourcePair{Cpu: "500m", Memory: "256Mi"},
				Limits:   &ResourcePair{Cpu: "2", Memory: "1Gi"},
				Devices: []*Device{
					{Type: "gpu", Count: 1, Vram: "8Gi"},
				},
			},
			Storage: []*Storage{
				{
					Name:        "data",
					Size:        "10Gi",
					Class:       "default",
					Replication: 3,
					MountPath:   "/var/lib/nginx",
					AccessMode:  AccessMode_ACCESS_MODE_RWO,
				},
			},
			Placement: &Placement{
				AntiAffinity:         AntiAffinity_ANTI_AFFINITY_NODE,
				NodeSelector:         map[string]string{"tier": "front"},
				RequiredCapabilities: []string{"kvm", "nvidia"},
				Tolerations:          []string{"storage-slow"},
				Spread:               Spread_SPREAD_EVEN,
			},
			Network: &Network{
				Ports: []*Port{
					{Name: "http", Port: 80, TargetPort: 8080, Protocol: "tcp", Expose: Expose_EXPOSE_VIP},
					{Name: "metrics", Port: 9100, TargetPort: 9100, Protocol: "tcp", Expose: Expose_EXPOSE_CLUSTER},
				},
				HealthCheck: &HealthCheck{
					Readiness: &HealthProbe{
						Type:                ProbeType_PROBE_HTTP,
						Path:                "/healthz",
						Port:                8080,
						InitialDelaySeconds: 5,
						PeriodSeconds:       10,
						TimeoutSeconds:      3,
						SuccessThreshold:    1,
						FailureThreshold:    3,
					},
					Liveness: &HealthProbe{
						Type:             ProbeType_PROBE_TCP,
						Port:             8080,
						PeriodSeconds:    30,
						FailureThreshold: 3,
						Command:          []string{},
					},
				},
			},
			Config: cfg,
			Secrets: []*SecretRef{
				{Name: "tls-cert", Path: "/run/secrets/tls"},
			},
			Backup: &Backup{
				Enabled:   true,
				Schedule:  "0 2 * * *",
				Retention: "30d",
				PreHook:   "",
				PostHook:  "",
			},
			DependsOn: []string{"db/cache"},
		},
		Status: &BlockStatus{
			Phase:              Phase_RUNNING,
			ObservedGeneration: 42,
			Replicas:           &StatusReplicas{Desired: 3, Ready: 3, Updated: 3},
			Conditions: []*Condition{
				{Type: "Ready", Status: true, Reason: "AllReplicasReady", Message: "3/3 ready", LastTransitionUnixSec: 1700000000},
			},
			Placements: []*PlacementStatus{
				{ReplicaIndex: 0, NodeId: "n1", Phase: Phase_RUNNING},
				{ReplicaIndex: 1, NodeId: "n2", Phase: Phase_RUNNING},
				{ReplicaIndex: 2, NodeId: "n3", Phase: Phase_RUNNING},
			},
			Message: "",
			PendingReason: &PendingReason{
				Code:    "AntiAffinityConflict",
				Message: "no node satisfies anti-affinity",
				PerNode: map[string]string{
					"n1": "anti-affinity: replica-0 here",
					"n2": "anti-affinity: replica-1 here",
					"n3": "cordoned",
				},
			},
		},
	}
}

func ptr[T any](v T) *T { return &v }

func TestBlockRoundTrip(t *testing.T) {
	orig := fullBlock(t)
	wire, err := pbproto.Marshal(orig)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := &Block{}
	if err := pbproto.Unmarshal(wire, got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !pbproto.Equal(orig, got) {
		t.Fatalf("round-trip changed the Block:\n want %+v\n got  %+v", orig, got)
	}

	// The Struct config must round-trip mixed scalar and nested types.
	root := got.GetSpec().GetConfig().AsMap()
	if root["serverName"] != "example.com" {
		t.Errorf("config serverName = %v, want example.com", root["serverName"])
	}
	if port, ok := root["port"].(float64); !ok || port != 8080 {
		t.Errorf("config port = %v (%T), want 8080", root["port"], root["port"])
	}
	aliases, ok := root["aliases"].([]any)
	if !ok || len(aliases) != 2 {
		t.Errorf("config aliases = %v, want 2 entries", root["aliases"])
	}
	limits, ok := root["limits"].(map[string]any)
	if !ok || limits["body"] != "1M" {
		t.Errorf("config limits = %v, want body=1M", root["limits"])
	}
}

// TestReplicasAbsentMeansAuto pins the optional-replicas semantics: an unset
// replicas field must survive the wire as "not set" (V6: daemonset may
// leave replicas unset/auto).
func TestReplicasAbsentMeansAuto(t *testing.T) {
	b := fullBlock(t)
	b.Spec.Replicas = nil
	wire, err := pbproto.Marshal(b)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := &Block{}
	if err := pbproto.Unmarshal(wire, got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.GetSpec().Replicas != nil {
		t.Fatalf("replicas = %v, want unset (auto)", got.GetSpec().GetReplicas())
	}
}

// TestEnumStrings pins stable wire names for one value of each Phase 04
// enum; logs and CLI output print these.
func TestEnumStrings(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"StrategyKind", StrategyKind_SINGLETON.String(), "SINGLETON"},
		{"StrategyKind", StrategyKind_PRIMARY_REPLICA.String(), "PRIMARY_REPLICA"},
		{"UpdateMode", UpdateMode_UPDATE_MODE_ROLLING.String(), "UPDATE_MODE_ROLLING"},
		{"Runtime", Runtime_RUNTIME_SYSTEMD.String(), "RUNTIME_SYSTEMD"},
		{"AntiAffinity", AntiAffinity_ANTI_AFFINITY_NODE.String(), "ANTI_AFFINITY_NODE"},
		{"Expose", Expose_EXPOSE_VIP.String(), "EXPOSE_VIP"},
		{"ProbeType", ProbeType_PROBE_HTTP.String(), "PROBE_HTTP"},
		{"AccessMode", AccessMode_ACCESS_MODE_RWX.String(), "ACCESS_MODE_RWX"},
		{"Spread", Spread_SPREAD_PACKED.String(), "SPREAD_PACKED"},
		{"Phase", Phase_PENDING.String(), "PENDING"},
		{"Phase", Phase_UPDATING.String(), "UPDATING"},
		{"EventType", EventType_EVENT_ADDED.String(), "EVENT_ADDED"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}
