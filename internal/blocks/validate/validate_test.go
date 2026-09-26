// Per-rule tests for the Phase 04 admission validation rules (T03).
//
// There is one test function per rule (TestV1..TestV24) so the rule -> test
// mapping is auditable at a glance (PHASE04.md §8, G4.1). Every rule has at
// least one accepting and one rejecting case; each rejecting case asserts
// the error message contains the substring required by the spec's
// "Message must mention" column.
package validate

import (
	"strings"
	"testing"

	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// fakeCatalog implements Catalog for tests; the real one lands in T04.
type fakeCatalog struct {
	types      []string
	configErrs func(t string, c *structpb.Struct) []string
}

func (f *fakeCatalog) HasType(t string) bool {
	for _, x := range f.types {
		if x == t {
			return true
		}
	}
	return false
}
func (f *fakeCatalog) Types() []string { return f.types }
func (f *fakeCatalog) ValidateConfig(t string, c *structpb.Struct) []string {
	if f.configErrs == nil {
		return nil
	}
	return f.configErrs(t, c)
}

func i32(i int32) *int32 { return &i }

// validBlock returns a block that passes all 24 rules against baseCtx().
func validBlock() *pb.Block {
	return &pb.Block{
		Metadata: &pb.Metadata{Name: "web", Namespace: "default"},
		Spec: &pb.BlockSpec{
			Type:     "web/nginx",
			Version:  "1.27",
			Replicas: i32(3),
			Strategy: &pb.Strategy{
				Kind: pb.StrategyKind_ACTIVE_ACTIVE,
				Update: &pb.UpdateStrategy{
					Mode:           pb.UpdateMode_UPDATE_MODE_ROLLING,
					MaxUnavailable: 1,
					MaxSurge:       0,
				},
			},
			Resources: &pb.Resources{
				Requests: &pb.ResourcePair{Cpu: "500m", Memory: "256Mi"},
				Limits:   &pb.ResourcePair{Cpu: "2", Memory: "1Gi"},
			},
			Storage: []*pb.Storage{{
				Name:        "data",
				Size:        "10Gi",
				Class:       "default",
				Replication: 3,
				MountPath:   "/var/lib/data",
				AccessMode:  pb.AccessMode_ACCESS_MODE_RWO,
			}},
			Placement: &pb.Placement{
				AntiAffinity:         pb.AntiAffinity_ANTI_AFFINITY_NONE,
				NodeSelector:         map[string]string{"tier": "front"},
				RequiredCapabilities: []string{"kvm"},
			},
			Network: &pb.Network{
				Ports: []*pb.Port{{
					Name: "http", Port: 80, TargetPort: 8080, Protocol: "tcp", Expose: pb.Expose_EXPOSE_CLUSTER,
				}},
				HealthCheck: &pb.HealthCheck{
					Readiness: &pb.HealthProbe{
						Type: pb.ProbeType_PROBE_HTTP, Path: "/healthz", Port: 8080,
					},
				},
			},
		},
	}
}

// baseCtx returns a Context under which validBlock() passes.
func baseCtx() Context {
	return Context{
		Catalog:           &fakeCatalog{types: []string{"web/nginx", "db/redis"}},
		SecretsExist:      func(names []string) []string { return nil },
		Existing:          []string{"other", "cache"},
		NodeCount:         5,
		KnownCapabilities: []string{"kvm", "nvidia"},
		Devices:           map[string]int32{"gpu": 4},
		SharedClasses:     map[string]bool{"cephfs": true},
		DependsOn:         map[string][]string{"db/cache": {}},
	}
}

func rules(errs []ValidationError) []string {
	out := make([]string, len(errs))
	for i, e := range errs {
		out[i] = e.Rule
	}
	return out
}

func byRule(errs []ValidationError, rule string) *ValidationError {
	for i := range errs {
		if errs[i].Rule == rule {
			return &errs[i]
		}
	}
	return nil
}

func TestV1NameDNS1123(t *testing.T) {
	b := validBlock()
	if errs := Validate(b, baseCtx()); byRule(errs, "V1") != nil {
		t.Fatalf("valid block failed V1: %v", byRule(errs, "V1"))
	}

	b = validBlock()
	b.Metadata.Name = "Web_1"
	errs := Validate(b, baseCtx())
	e := byRule(errs, "V1")
	if e == nil {
		t.Fatalf("expected V1 failure for name %q", "Web_1")
	}
	// V1: message must name the invalid character.
	if !strings.Contains(e.Message, "W") || !strings.Contains(e.Message, "_") {
		t.Errorf("V1 message %q does not name the invalid character(s)", e.Message)
	}

	b = validBlock()
	b.Metadata.Name = strings.Repeat("a", 64)
	if e := byRule(Validate(b, baseCtx()), "V1"); e == nil || !strings.Contains(e.Message, "63") {
		t.Errorf("V1 length check failed: %v", e)
	}

	b = validBlock()
	b.Metadata.Name = ""
	if e := byRule(Validate(b, baseCtx()), "V1"); e == nil {
		t.Error("V1 empty name check failed")
	}

	b = validBlock()
	b.Metadata.Namespace = "-bad"
	if e := byRule(Validate(b, baseCtx()), "V1"); e == nil || !strings.Contains(e.Message, "metadata.namespace") {
		t.Errorf("V1 namespace check failed: %v", e)
	}
}

func TestV2NameUniqueInNamespace(t *testing.T) {
	ctx := baseCtx()
	ctx.Existing = []string{"other"}
	if errs := Validate(validBlock(), ctx); byRule(errs, "V2") != nil {
		t.Fatalf("valid block failed V2: %v", byRule(errs, "V2"))
	}

	ctx.Existing = []string{"other", "web"}
	e := byRule(Validate(validBlock(), ctx), "V2")
	if e == nil {
		t.Fatal("expected V2 failure for duplicate name")
	}
	// V2: message must name the conflicting block.
	if !strings.Contains(e.Message, `conflicts with existing block "web"`) {
		t.Errorf("V2 message %q does not name the conflicting block", e.Message)
	}
}

func TestV3TypeExistsInCatalog(t *testing.T) {
	ctx := baseCtx()
	if errs := Validate(validBlock(), ctx); byRule(errs, "V3") != nil {
		t.Fatalf("valid block failed V3: %v", byRule(errs, "V3"))
	}

	b := validBlock()
	b.Spec.Type = "web/nope"
	e := byRule(Validate(b, ctx), "V3")
	if e == nil {
		t.Fatal("expected V3 failure for unknown type")
	}
	// V3: message must name the available types.
	if !strings.Contains(e.Message, "web/nope") || !strings.Contains(e.Message, "db/redis") {
		t.Errorf("V3 message %q does not mention available types", e.Message)
	}
}

func TestV4ReplicasNonNegative(t *testing.T) {
	b := validBlock()
	b.Spec.Replicas = i32(0)
	if errs := Validate(b, baseCtx()); byRule(errs, "V4") != nil {
		t.Fatalf("replicas 0 failed V4: %v", byRule(errs, "V4"))
	}

	b = validBlock()
	b.Spec.Replicas = i32(-1)
	e := byRule(Validate(b, baseCtx()), "V4")
	if e == nil {
		t.Fatal("expected V4 failure for negative replicas")
	}
	if !strings.Contains(e.Message, "-1") || !strings.Contains(e.Message, ">= 0") {
		t.Errorf("V4 message %q does not state the constraint", e.Message)
	}
}

func TestV5SingletonReplicasOne(t *testing.T) {
	b := validBlock()
	b.Spec.Strategy.Kind = pb.StrategyKind_SINGLETON
	b.Spec.Replicas = i32(1)
	if errs := Validate(b, baseCtx()); byRule(errs, "V5") != nil {
		t.Fatalf("singleton replicas=1 failed V5: %v", byRule(errs, "V5"))
	}

	b.Spec.Replicas = i32(3)
	e := byRule(Validate(b, baseCtx()), "V5")
	if e == nil {
		t.Fatal("expected V5 failure for singleton with replicas 3")
	}
	if !strings.Contains(e.Message, "3") {
		t.Errorf("V5 message %q does not name the replica count", e.Message)
	}

	// Unset replicas (auto) is fine for singleton too.
	b.Spec.Replicas = nil
	if errs := Validate(b, baseCtx()); byRule(errs, "V5") != nil {
		t.Fatalf("singleton auto replicas failed V5: %v", byRule(errs, "V5"))
	}
}

func TestV6DaemonsetReplicasUnset(t *testing.T) {
	b := validBlock()
	b.Spec.Strategy.Kind = pb.StrategyKind_DAEMONSET
	b.Spec.Replicas = nil
	if errs := Validate(b, baseCtx()); byRule(errs, "V6") != nil {
		t.Fatalf("daemonset auto failed V6: %v", byRule(errs, "V6"))
	}

	b.Spec.Replicas = i32(4)
	e := byRule(Validate(b, baseCtx()), "V6")
	if e == nil {
		t.Fatal("expected V6 failure for daemonset with explicit replicas")
	}
	if !strings.Contains(e.Message, "unset") || !strings.Contains(e.Message, "4") {
		t.Errorf("V6 message %q does not state the auto requirement", e.Message)
	}
}

func TestV7PrimaryReplicaMinTwo(t *testing.T) {
	b := validBlock()
	b.Spec.Strategy.Kind = pb.StrategyKind_PRIMARY_REPLICA
	b.Spec.Replicas = i32(2)
	if errs := Validate(b, baseCtx()); byRule(errs, "V7") != nil {
		t.Fatalf("primary-replica replicas=2 failed V7: %v", byRule(errs, "V7"))
	}

	b.Spec.Replicas = i32(1)
	e := byRule(Validate(b, baseCtx()), "V7")
	if e == nil {
		t.Fatal("expected V7 failure for primary-replica with replicas 1")
	}
	if !strings.Contains(e.Message, ">= 2") {
		t.Errorf("V7 message %q does not state the >= 2 requirement", e.Message)
	}
}

func TestV8LimitsGTE(t *testing.T) {
	if errs := Validate(validBlock(), baseCtx()); byRule(errs, "V8") != nil {
		t.Fatalf("valid block failed V8: %v", byRule(errs, "V8"))
	}

	b := validBlock()
	b.Spec.Resources.Limits.Memory = "256Mi"
	b.Spec.Resources.Requests.Memory = "1Gi"
	e := byRule(Validate(b, baseCtx()), "V8")
	if e == nil {
		t.Fatal("expected V8 failure for limits < requests")
	}
	// V8: message must name which resource.
	if !strings.Contains(e.Message, `"memory"`) || !strings.Contains(e.Message, "256Mi") {
		t.Errorf("V8 message %q does not name the resource", e.Message)
	}

	b = validBlock()
	b.Spec.Resources.Limits.Cpu = "100m"
	b.Spec.Resources.Requests.Cpu = "500m"
	if e := byRule(Validate(b, baseCtx()), "V8"); e == nil || !strings.Contains(e.Message, `"cpu"`) {
		t.Errorf("V8 cpu check failed: %v", e)
	}
}

func TestV9QuantitiesParse(t *testing.T) {
	if errs := Validate(validBlock(), baseCtx()); byRule(errs, "V9") != nil {
		t.Fatalf("valid block failed V9: %v", byRule(errs, "V9"))
	}

	b := validBlock()
	b.Spec.Resources.Requests.Cpu = "50 0m"
	e := byRule(Validate(b, baseCtx()), "V9")
	if e == nil {
		t.Fatal("expected V9 failure for unparseable cpu")
	}
	// V9: message must name the bad value (and, helpfully, the field).
	if !strings.Contains(e.Message, "50 0m") || !strings.Contains(e.Message, "requests.cpu") {
		t.Errorf("V9 message %q does not name the bad value", e.Message)
	}

	b = validBlock()
	b.Spec.Resources.Limits.Memory = "1.5Gi" // byte quantities must be whole
	if e := byRule(Validate(b, baseCtx()), "V9"); e == nil || !strings.Contains(e.Message, "limits.memory") {
		t.Errorf("V9 memory check failed: %v", e)
	}
}

func TestV10Ports(t *testing.T) {
	if errs := Validate(validBlock(), baseCtx()); byRule(errs, "V10") != nil {
		t.Fatalf("valid block failed V10: %v", byRule(errs, "V10"))
	}

	b := validBlock()
	b.Spec.Network.Ports = append(b.Spec.Network.Ports,
		&pb.Port{Name: "alt", Port: 80, TargetPort: 8081, Expose: pb.Expose_EXPOSE_NONE})
	e := byRule(Validate(b, baseCtx()), "V10")
	if e == nil {
		t.Fatal("expected V10 failure for duplicate port")
	}
	// V10: message must name the duplicate.
	if !strings.Contains(e.Message, "80") {
		t.Errorf("V10 message %q does not name the duplicate", e.Message)
	}

	b = validBlock()
	b.Spec.Network.Ports = append(b.Spec.Network.Ports,
		&pb.Port{Name: "http", Port: 81, TargetPort: 8081, Expose: pb.Expose_EXPOSE_NONE})
	if e := byRule(Validate(b, baseCtx()), "V10"); e == nil || !strings.Contains(e.Message, "http") {
		t.Errorf("V10 duplicate-name check failed: %v", e)
	}

	b = validBlock()
	b.Spec.Network.Ports[0].Port = 70000
	if e := byRule(Validate(b, baseCtx()), "V10"); e == nil || !strings.Contains(e.Message, "70000") {
		t.Errorf("V10 range check failed: %v", e)
	}
}

func TestV11ExposeVIPRequiresReadiness(t *testing.T) {
	b := validBlock()
	b.Spec.Network.Ports[0].Expose = pb.Expose_EXPOSE_VIP
	if errs := Validate(b, baseCtx()); byRule(errs, "V11") != nil {
		t.Fatalf("vip with readiness failed V11: %v", byRule(errs, "V11"))
	}

	b.Spec.Network.HealthCheck = nil
	e := byRule(Validate(b, baseCtx()), "V11")
	if e == nil {
		t.Fatal("expected V11 failure for vip without readiness")
	}
	if !strings.Contains(e.Message, "readiness") {
		t.Errorf("V11 message %q does not mention readiness", e.Message)
	}
}

func TestV12Storage(t *testing.T) {
	ctx := baseCtx()
	if errs := Validate(validBlock(), ctx); byRule(errs, "V12") != nil {
		t.Fatalf("valid block failed V12: %v", byRule(errs, "V12"))
	}

	b := validBlock()
	b.Spec.Storage[0].Size = "10 Q"
	if e := byRule(Validate(b, ctx), "V12"); e == nil || !strings.Contains(e.Message, "10 Q") {
		t.Errorf("V12 size parse check failed: %v", e)
	}

	b = validBlock()
	b.Spec.Storage[0].Replication = 7
	if e := byRule(Validate(b, ctx), "V12"); e == nil || !strings.Contains(e.Message, "1-5") {
		t.Errorf("V12 replication range check failed: %v", e)
	}

	// Unset replication means "the class's, clamped to the nodes there are".
	b = validBlock()
	b.Spec.Storage[0].Replication = 0
	if e := byRule(Validate(b, ctx), "V12"); e != nil {
		t.Errorf("unset replication rejected: %v", e)
	}

	// replication <= node count: NodeCount 3 < replication 4 names the count
	// (4 is inside the valid 1-5 range, so the range check doesn't fire first).
	ctx = baseCtx()
	ctx.NodeCount = 3
	b.Spec.Storage[0].Replication = 4
	e := byRule(Validate(b, ctx), "V12")
	if e == nil || !strings.Contains(e.Message, "node count 3") {
		t.Errorf("V12 node-count check failed: %v", e)
	}
}

func TestV13SharedClassForRWX(t *testing.T) {
	ctx := baseCtx()
	if errs := Validate(validBlock(), ctx); byRule(errs, "V13") != nil {
		t.Fatalf("valid block failed V13: %v", byRule(errs, "V13"))
	}

	b := validBlock()
	b.Spec.Storage[0].AccessMode = pb.AccessMode_ACCESS_MODE_RWX
	e := byRule(Validate(b, ctx), "V13")
	if e == nil {
		t.Fatal("expected V13 failure for rwx on non-shared class")
	}
	// V13: message names the volume/class and the shared-capable list.
	if !strings.Contains(e.Message, "cephfs") {
		t.Errorf("V13 message %q does not mention available shared classes", e.Message)
	}

	b.Spec.Storage[0].Class = "cephfs"
	if errs := Validate(b, ctx); byRule(errs, "V13") != nil {
		t.Fatalf("rwx on shared class failed V13: %v", byRule(errs, "V13"))
	}
}

func TestV14HTTPProbeNeedsPathAndPort(t *testing.T) {
	if errs := Validate(validBlock(), baseCtx()); byRule(errs, "V14") != nil {
		t.Fatalf("valid block failed V14: %v", byRule(errs, "V14"))
	}

	b := validBlock()
	b.Spec.Network.HealthCheck.Readiness.Path = ""
	e := byRule(Validate(b, baseCtx()), "V14")
	if e == nil {
		t.Fatal("expected V14 failure for http probe without path")
	}
	if !strings.Contains(e.Message, "readiness") || !strings.Contains(e.Message, "path") {
		t.Errorf("V14 message %q does not name the probe and missing field", e.Message)
	}

	b = validBlock()
	b.Spec.Network.HealthCheck.Liveness = &pb.HealthProbe{Type: pb.ProbeType_PROBE_HTTP, Port: 8080}
	if e := byRule(Validate(b, baseCtx()), "V14"); e == nil || !strings.Contains(e.Message, "path") {
		t.Errorf("V14 liveness path check failed: %v", e)
	}
}

func TestV15ExecProbeNeedsCommand(t *testing.T) {
	if errs := Validate(validBlock(), baseCtx()); byRule(errs, "V15") != nil {
		t.Fatalf("valid block failed V15: %v", byRule(errs, "V15"))
	}

	b := validBlock()
	b.Spec.Network.HealthCheck.Liveness = &pb.HealthProbe{Type: pb.ProbeType_PROBE_EXEC}
	e := byRule(Validate(b, baseCtx()), "V15")
	if e == nil {
		t.Fatal("expected V15 failure for exec probe without command")
	}
	if !strings.Contains(e.Message, "command") {
		t.Errorf("V15 message %q does not mention command", e.Message)
	}

	b.Spec.Network.HealthCheck.Liveness.Command = []string{"/bin/check"}
	if errs := Validate(b, baseCtx()); byRule(errs, "V15") != nil {
		t.Fatalf("exec probe with command failed V15: %v", byRule(errs, "V15"))
	}
}

// V16 was removed: replicas > node count under strict node
// anti-affinity must be ACCEPTED (the surplus replica goes Pending at
// placement time, §8 block-antiaffinity).
func TestV16AntiAffinityOverCountAccepted(t *testing.T) {
	ctx := baseCtx()
	b := validBlock()
	b.Spec.Placement.AntiAffinity = pb.AntiAffinity_ANTI_AFFINITY_NODE
	b.Spec.Replicas = i32(6)
	if errs := Validate(b, ctx); len(errs) != 0 {
		t.Fatalf("anti-affinity replicas > node count rejected: %v", errs)
	}
}

func TestV17NodeSelectorLabelKeys(t *testing.T) {
	if errs := Validate(validBlock(), baseCtx()); byRule(errs, "V17") != nil {
		t.Fatalf("valid block failed V17: %v", byRule(errs, "V17"))
	}

	b := validBlock()
	b.Spec.Placement.NodeSelector = map[string]string{"Tier!": "front"}
	e := byRule(Validate(b, baseCtx()), "V17")
	if e == nil {
		t.Fatal("expected V17 failure for invalid label key")
	}
	if !strings.Contains(e.Message, "Tier!") {
		t.Errorf("V17 message %q does not name the bad key", e.Message)
	}

	b = validBlock()
	b.Spec.Placement.NodeSelector = map[string]string{"example.com/tier": "front"}
	if errs := Validate(b, baseCtx()); byRule(errs, "V17") != nil {
		t.Fatalf("prefixed label key failed V17: %v", byRule(errs, "V17"))
	}
}

func TestV18KnownCapabilities(t *testing.T) {
	ctx := baseCtx()
	if errs := Validate(validBlock(), ctx); byRule(errs, "V18") != nil {
		t.Fatalf("valid block failed V18: %v", byRule(errs, "V18"))
	}

	b := validBlock()
	b.Spec.Placement.RequiredCapabilities = []string{"tpu"}
	e := byRule(Validate(b, ctx), "V18")
	if e == nil {
		t.Fatal("expected V18 failure for unknown capability")
	}
	// V18: message must name the known list.
	if !strings.Contains(e.Message, "tpu") || !strings.Contains(e.Message, "kvm, nvidia") {
		t.Errorf("V18 message %q does not name the known list", e.Message)
	}
}

func TestV19ConfigSchema(t *testing.T) {
	ctx := baseCtx()
	if errs := Validate(validBlock(), ctx); byRule(errs, "V19") != nil {
		t.Fatalf("valid block failed V19: %v", byRule(errs, "V19"))
	}

	ctx.Catalog = &fakeCatalog{
		types: []string{"web/nginx"},
		configErrs: func(t string, c *structpb.Struct) []string {
			return []string{"serverName"}
		},
	}
	e := byRule(Validate(validBlock(), ctx), "V19")
	if e == nil {
		t.Fatal("expected V19 failure for config schema violation")
	}
	// V19: message must name the failing field path.
	if !strings.Contains(e.Message, "serverName") {
		t.Errorf("V19 message %q does not name the failing field path", e.Message)
	}
}

func TestV20SecretsExist(t *testing.T) {
	ctx := baseCtx()
	b := validBlock()
	b.Spec.Secrets = []*pb.SecretRef{{Name: "tls-cert", Path: "/run/secrets/tls"}}
	if errs := Validate(b, ctx); byRule(errs, "V20") != nil {
		t.Fatalf("existing secret failed V20: %v", byRule(errs, "V20"))
	}

	ctx.SecretsExist = func(names []string) []string { return []string{"tls-cert"} }
	e := byRule(Validate(b, ctx), "V20")
	if e == nil {
		t.Fatal("expected V20 failure for missing secret")
	}
	// V20: message must name the missing secret.
	if !strings.Contains(e.Message, "tls-cert") {
		t.Errorf("V20 message %q does not name the missing secret", e.Message)
	}

	// nil SecretsExist is permissive by contract (TODO: real secrets store).
	ctx2 := baseCtx()
	ctx2.SecretsExist = nil
	if errs := Validate(b, ctx2); byRule(errs, "V20") != nil {
		t.Fatalf("nil SecretsExist should be permissive: %v", byRule(errs, "V20"))
	}
}

func TestV21CronSchedule(t *testing.T) {
	if errs := Validate(validBlock(), baseCtx()); byRule(errs, "V21") != nil {
		t.Fatalf("valid block failed V21: %v", byRule(errs, "V21"))
	}

	b := validBlock()
	b.Spec.Backup = &pb.Backup{Enabled: true, Schedule: "every day"}
	e := byRule(Validate(b, baseCtx()), "V21")
	if e == nil {
		t.Fatal("expected V21 failure for invalid cron schedule")
	}
	if !strings.Contains(e.Message, "every day") {
		t.Errorf("V21 message %q does not name the bad schedule", e.Message)
	}

	b.Spec.Backup.Schedule = "0 2 * * *"
	if errs := Validate(b, baseCtx()); byRule(errs, "V21") != nil {
		t.Fatalf("valid cron failed V21: %v", byRule(errs, "V21"))
	}

	// Disabled backup: schedule not checked.
	b.Spec.Backup = &pb.Backup{Enabled: false, Schedule: "not a cron"}
	if errs := Validate(b, baseCtx()); byRule(errs, "V21") != nil {
		t.Fatalf("disabled backup should skip V21: %v", byRule(errs, "V21"))
	}
}

func TestV22MaxUnavailablePlusSurge(t *testing.T) {
	if errs := Validate(validBlock(), baseCtx()); byRule(errs, "V22") != nil {
		t.Fatalf("valid block failed V22: %v", byRule(errs, "V22"))
	}

	b := validBlock()
	b.Spec.Strategy.Update.MaxUnavailable = 0
	b.Spec.Strategy.Update.MaxSurge = 0
	e := byRule(Validate(b, baseCtx()), "V22")
	if e == nil {
		t.Fatal("expected V22 failure for 0+0 rolling update")
	}
	if !strings.Contains(e.Message, ">= 1") {
		t.Errorf("V22 message %q does not state the >= 1 requirement", e.Message)
	}

	b.Spec.Strategy.Update.MaxSurge = 1
	if errs := Validate(b, baseCtx()); byRule(errs, "V22") != nil {
		t.Fatalf("1+1 rolling failed V22: %v", byRule(errs, "V22"))
	}

	// Non-rolling modes are not subject to the rule.
	b.Spec.Strategy.Update.Mode = pb.UpdateMode_UPDATE_MODE_RECREATE
	b.Spec.Strategy.Update.MaxSurge = 0
	if errs := Validate(b, baseCtx()); byRule(errs, "V22") != nil {
		t.Fatalf("recreate mode should skip V22: %v", byRule(errs, "V22"))
	}
}

func TestV23DevicesAvailable(t *testing.T) {
	ctx := baseCtx()
	if errs := Validate(validBlock(), ctx); byRule(errs, "V23") != nil {
		t.Fatalf("valid block failed V23: %v", byRule(errs, "V23"))
	}

	b := validBlock()
	b.Spec.Resources.Devices = []*pb.Device{{Type: "tpu", Count: 1}}
	e := byRule(Validate(b, ctx), "V23")
	if e == nil {
		t.Fatal("expected V23 failure for unavailable device")
	}
	// V23: message must name what's available.
	if !strings.Contains(e.Message, "tpu") || !strings.Contains(e.Message, "gpu (4)") {
		t.Errorf("V23 message %q does not name what is available", e.Message)
	}

	b.Spec.Resources.Devices = []*pb.Device{{Type: "gpu", Count: 1, Vram: "8Gi"}}
	if errs := Validate(b, ctx); byRule(errs, "V23") != nil {
		t.Fatalf("gpu with vram failed V23: %v", byRule(errs, "V23"))
	}

	b.Spec.Resources.Devices = []*pb.Device{{Type: "gpu", Count: 1, Vram: "8Q"}}
	if e := byRule(Validate(b, ctx), "V23"); e == nil || !strings.Contains(e.Message, "8Q") {
		t.Errorf("V23 vram parse check failed: %v", e)
	}
}

func TestV24DependsOnCycle(t *testing.T) {
	ctx := baseCtx()
	if errs := Validate(validBlock(), ctx); byRule(errs, "V24") != nil {
		t.Fatalf("valid block failed V24: %v", byRule(errs, "V24"))
	}

	b := validBlock()
	b.Spec.DependsOn = []string{"db/cache"}
	ctx.DependsOn = map[string][]string{"db/cache": {"web"}}
	e := byRule(Validate(b, ctx), "V24")
	if e == nil {
		t.Fatal("expected V24 failure for dependency cycle")
	}
	// V24: message must name the cycle.
	if !strings.Contains(e.Message, "web -> db/cache -> web") {
		t.Errorf("V24 message %q does not name the cycle", e.Message)
	}

	// Acyclic dependency through an existing block is fine.
	ctx.DependsOn = map[string][]string{"db/cache": {"db/base"}}
	ctx.DependsOn["db/base"] = []string{}
	if errs := Validate(b, ctx); byRule(errs, "V24") != nil {
		t.Fatalf("acyclic deps failed V24: %v", byRule(errs, "V24"))
	}
}

func TestValidBlockPassesAllRules(t *testing.T) {
	if errs := Validate(validBlock(), baseCtx()); len(errs) != 0 {
		t.Fatalf("valid block should produce no errors, got: %v", errs)
	}
}

func TestRuleOrderDeterministic(t *testing.T) {
	b := validBlock()
	b.Metadata.Name = "Web_1" // V1
	b.Spec.Type = "web/nope"  // V3
	b.Spec.Replicas = i32(-1) // V4
	got := rules(Validate(b, baseCtx()))
	want := []string{"V1", "V3", "V4"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("rule order = %v, want %v", got, want)
	}
}
