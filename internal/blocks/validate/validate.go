// Package validate implements the Phase 04 admission validation rules
// (PHASE04.md §3.2, V1–V24). Validation runs at API write time, before
// anything is persisted — rejecting early is the whole point.
//
// Every rule has a stable identifier ("V1".."V24") so API consumers and
// tests can map failures back to the spec table. Each rule's error message
// satisfies the "Message must mention" column of the spec (e.g. V1 names
// the invalid character, V16 names the node count).
//
// Rules that need knowledge beyond the block itself read it from Context:
//
//	V3/V19   Catalog (type existence, config JSON-Schema validation)
//	V20      Context.SecretsExist callback
//	V2       Context.Existing (blocks already in the namespace)
//	V12/V16  Context.NodeCount
//	V13/V18/V23  Context.SharedClasses / KnownCapabilities / Devices
//	V24      Context.DependsOn (existing blocks' dependency edges)
//
// TODO(Phase 06): a real secrets store for V20 — Context.SecretsExist is a
// callback, nil means permissive. This is an open TODO, not a silent gap.
package validate

import (
	"fmt"
	"sort"
	"strings"

	"github.com/expanse/expanse/internal/quantity"
	pb "github.com/expanse/expanse/proto"
	"github.com/robfig/cron/v3"
	"google.golang.org/protobuf/types/known/structpb"
)

// ValidationError is one failed validation rule. Rule is the stable spec
// identifier ("V1".."V24"); Message satisfies the spec's "Message must
// mention" column for that rule.
type ValidationError struct {
	Rule    string
	Message string
}

func (e ValidationError) Error() string { return e.Rule + ": " + e.Message }

// Catalog is the minimal view of the block catalog that validation needs
// (T04 provides the real implementation; tests use a fake).
type Catalog interface {
	// HasType reports whether the catalog knows block type t ("<category>/<name>").
	HasType(t string) bool
	// Types lists the available types (V3's message names them).
	Types() []string
	// ValidateConfig checks config against the type's JSON Schema and
	// returns one string per failing field path (V19).
	ValidateConfig(t string, config *structpb.Struct) []string
}

// Context carries cluster state into validation. Zero fields mean "not
// known" and the corresponding rule comparisons are skipped rather than
// failed, so callers never fabricate cluster facts.
type Context struct {
	// Catalog backs V3/V19. Admission must always wire a real catalog;
	// nil skips V3/V19 (used only by tests of catalog-free paths).
	Catalog Catalog
	// SecretsExist returns the subset of names that do not exist. nil is
	// permissive (TODO(Phase 06): real secrets store).
	SecretsExist func(names []string) []string
	// Existing lists block names already present in the same namespace,
	// excluding the block being validated (V2).
	Existing []string
	// NodeCount is the current cluster size (V12, V16). 0 skips the
	// node-count comparisons.
	NodeCount int
	// KnownCapabilities lists the capability strings any node in the
	// cluster advertises (V18).
	KnownCapabilities []string
	// Devices maps device type -> count available cluster-wide (V23).
	Devices map[string]int32
	// SharedClasses marks storage classes that support multi-node (rwx)
	// access (V13).
	SharedClasses map[string]bool
	// DependsOn maps existing block name -> its spec.dependsOn list,
	// for cycle detection (V24) through already-admitted blocks.
	DependsOn map[string][]string
}

// Validate runs all 24 rules in spec order and returns every failure.
func Validate(b *pb.Block, ctx Context) []ValidationError {
	var errs []ValidationError
	add := func(es []ValidationError) { errs = append(errs, es...) }
	add(v1(b))
	add(v2(b, ctx))
	add(v3(b, ctx))
	add(v4(b))
	add(v5(b))
	add(v6(b))
	add(v7(b))
	add(v8(b))
	add(v9(b))
	add(v10(b))
	add(v11(b))
	add(v12(b, ctx))
	add(v13(b, ctx))
	add(v14(b))
	add(v15(b))
	add(v16(b, ctx))
	add(v17(b))
	add(v18(b, ctx))
	add(v19(b, ctx))
	add(v20(b, ctx))
	add(v21(b))
	add(v22(b))
	add(v23(b, ctx))
	add(v24(b, ctx))
	return errs
}

func verr(rule, format string, args ...any) ValidationError {
	return ValidationError{Rule: rule, Message: fmt.Sprintf(format, args...)}
}

// --- V1: name is DNS-1123, <= 63 chars; message names the invalid character ---

// dnsLabelBadChar returns the first offending rune of s ("" if s is fine),
// or "length" when only the 63-char cap is violated.
func dnsLabelBadChar(s string) string {
	if len(s) == 0 {
		return "empty"
	}
	if len(s) > 63 {
		return "length"
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			return string(r) + fmt.Sprintf(" (byte %d)", i)
		}
	}
	if s[0] == '-' || s[len(s)-1] == '-' {
		return string(s[0]) + "/" + string(s[len(s)-1]) + " (leading/trailing dash)"
	}
	return ""
}

func v1(b *pb.Block) []ValidationError {
	var errs []ValidationError
	for field, name := range map[string]string{
		"metadata.name":      b.GetMetadata().GetName(),
		"metadata.namespace": b.GetMetadata().GetNamespace(),
	} {
		if bad := dnsLabelBadChar(name); bad != "" {
			if bad == "length" {
				errs = append(errs, verr("V1",
					"%s %q is %d characters; DNS-1123 labels are at most 63", field, name, len(name)))
				continue
			}
			if bad == "empty" {
				errs = append(errs, verr("V1", "%s must not be empty (DNS-1123 label, 1-63 chars)", field))
				continue
			}
			errs = append(errs, verr("V1",
				"%s %q is not a valid DNS-1123 label: character %s is not allowed (lowercase alphanumerics, \"-\", 1-63 chars)", field, name, bad))
		}
	}
	return errs
}

// --- V2: name unique within namespace; message names the conflicting block ---

func v2(b *pb.Block, ctx Context) []ValidationError {
	name := b.GetMetadata().GetName()
	for _, existing := range ctx.Existing {
		if existing == name {
			return []ValidationError{verr("V2",
				"block name %q in namespace %q conflicts with existing block %q", name, b.GetMetadata().GetNamespace(), existing)}
		}
	}
	return nil
}

// --- V3: type exists in the catalog; message names available types ---

func v3(b *pb.Block, ctx Context) []ValidationError {
	if ctx.Catalog == nil {
		return nil
	}
	typ := b.GetSpec().GetType()
	if ctx.Catalog.HasType(typ) {
		return nil
	}
	avail := ctx.Catalog.Types()
	sort.Strings(avail)
	return []ValidationError{verr("V3",
		"block type %q not found in the catalog; available types: %s", typ, strings.Join(avail, ", "))}
}

// --- V4: replicas >= 0 ---

func v4(b *pb.Block) []ValidationError {
	if r := b.GetSpec().Replicas; r != nil && *r < 0 {
		return []ValidationError{verr("V4", "spec.replicas is %d, must be >= 0", *r)}
	}
	return nil
}

// --- V5: singleton => replicas == 1 ---

func v5(b *pb.Block) []ValidationError {
	if b.GetSpec().GetStrategy().GetKind() == pb.StrategyKind_SINGLETON {
		if r := b.GetSpec().Replicas; r != nil && *r != 1 {
			return []ValidationError{verr("V5",
				"strategy singleton requires replicas == 1, got %d", *r)}
		}
	}
	return nil
}

// --- V6: daemonset => replicas unset/auto ---

func v6(b *pb.Block) []ValidationError {
	if b.GetSpec().GetStrategy().GetKind() == pb.StrategyKind_DAEMONSET && b.GetSpec().Replicas != nil {
		return []ValidationError{verr("V6",
			"strategy daemonset requires replicas to be unset (auto), got %d", *b.GetSpec().Replicas)}
	}
	return nil
}

// --- V7: primary-replica => replicas >= 2 ---

func v7(b *pb.Block) []ValidationError {
	if b.GetSpec().GetStrategy().GetKind() == pb.StrategyKind_PRIMARY_REPLICA {
		if r := b.GetSpec().Replicas; r != nil && *r < 2 {
			return []ValidationError{verr("V7",
				"strategy primary-replica requires replicas >= 2, got %d", *r)}
		}
	}
	return nil
}

// --- V8: limits >= requests for every resource; message names the resource ---

func v8(b *pb.Block) []ValidationError {
	res := b.GetSpec().GetResources()
	if res == nil {
		return nil
	}
	var errs []ValidationError
	// Parse failures are V9's job; V8 only compares values that parse.
	if reqCPU, err1 := quantity.ParseCPU(res.GetRequests().GetCpu()); err1 == nil {
		if limCPU, err2 := quantity.ParseCPU(res.GetLimits().GetCpu()); err2 == nil && limCPU.LT(reqCPU) {
			errs = append(errs, verr("V8",
				"resource \"cpu\": limit %s is less than request %s", limCPU, reqCPU))
		}
	}
	if reqMem, err1 := quantity.ParseBytes(res.GetRequests().GetMemory()); err1 == nil {
		if limMem, err2 := quantity.ParseBytes(res.GetLimits().GetMemory()); err2 == nil && limMem.LT(reqMem) {
			errs = append(errs, verr("V8",
				"resource \"memory\": limit %s is less than request %s", limMem, reqMem))
		}
	}
	return errs
}

// --- V9: cpu/memory quantities parse; message names the bad value ---

func v9(b *pb.Block) []ValidationError {
	res := b.GetSpec().GetResources()
	if res == nil {
		return nil
	}
	var errs []ValidationError
	checkCPU := func(field, val string) {
		if val == "" {
			return
		}
		if _, err := quantity.ParseCPU(val); err != nil {
			errs = append(errs, verr("V9", "spec.resources.%s: %v", field, err))
		}
	}
	checkBytes := func(field, val string) {
		if val == "" {
			return
		}
		if _, err := quantity.ParseBytes(val); err != nil {
			errs = append(errs, verr("V9", "spec.resources.%s: %v", field, err))
		}
	}
	checkCPU("requests.cpu", res.GetRequests().GetCpu())
	checkBytes("requests.memory", res.GetRequests().GetMemory())
	checkCPU("limits.cpu", res.GetLimits().GetCpu())
	checkBytes("limits.memory", res.GetLimits().GetMemory())
	return errs
}

// --- V10: ports 1-65535, unique names, unique ports; message names the duplicate ---

func v10(b *pb.Block) []ValidationError {
	var errs []ValidationError
	seenName, seenPort := map[string]bool{}, map[int32]string{}
	for _, p := range b.GetSpec().GetNetwork().GetPorts() {
		if p.GetPort() < 1 || p.GetPort() > 65535 {
			errs = append(errs, verr("V10", "port %q has port %d, must be 1-65535", p.GetName(), p.GetPort()))
		}
		if p.GetTargetPort() < 1 || p.GetTargetPort() > 65535 {
			errs = append(errs, verr("V10", "port %q has targetPort %d, must be 1-65535", p.GetName(), p.GetTargetPort()))
		}
		if seenName[p.GetName()] {
			errs = append(errs, verr("V10", "duplicate port name %q", p.GetName()))
		}
		seenName[p.GetName()] = true
		if first := seenPort[p.GetPort()]; first != "" {
			errs = append(errs, verr("V10",
				"duplicate port %d (already used by port %q)", p.GetPort(), first))
		}
		seenPort[p.GetPort()] = p.GetName()
	}
	return errs
}

// --- V11: expose vip requires a readiness check ---

func v11(b *pb.Block) []ValidationError {
	readiness := b.GetSpec().GetNetwork().GetHealthCheck().GetReadiness()
	var errs []ValidationError
	for _, p := range b.GetSpec().GetNetwork().GetPorts() {
		if p.GetExpose() == pb.Expose_EXPOSE_VIP &&
			(readiness == nil || readiness.GetType() == pb.ProbeType_PROBE_TYPE_UNSPECIFIED || readiness.GetType() == pb.ProbeType_PROBE_NONE) {
			errs = append(errs, verr("V11",
				"port %q exposes a VIP, which requires a readiness check (spec.network.healthCheck.readiness)", p.GetName()))
		}
	}
	return errs
}

// --- V12: storage sizes parse; replication 1-5; <= cluster node count ---

func v12(b *pb.Block, ctx Context) []ValidationError {
	var errs []ValidationError
	for _, s := range b.GetSpec().GetStorage() {
		if _, err := quantity.ParseBytes(s.GetSize()); err != nil {
			errs = append(errs, verr("V12",
				"storage volume %q: invalid size: %v", s.GetName(), err))
		}
		if r := s.GetReplication(); r < 1 || r > 5 {
			errs = append(errs, verr("V12",
				"storage volume %q has replication %d, must be 1-5", s.GetName(), r))
		}
		if ctx.NodeCount > 0 && s.GetReplication() > int32(ctx.NodeCount) {
			errs = append(errs, verr("V12",
				"storage volume %q has replication %d, exceeds cluster node count %d", s.GetName(), s.GetReplication(), ctx.NodeCount))
		}
	}
	return errs
}

// --- V13: accessMode rwx requires a shared-capable storage class ---

func v13(b *pb.Block, ctx Context) []ValidationError {
	var errs []ValidationError
	for _, s := range b.GetSpec().GetStorage() {
		if s.GetAccessMode() != pb.AccessMode_ACCESS_MODE_RWX {
			continue
		}
		class := s.GetClass()
		if class == "" || !ctx.SharedClasses[class] {
			avail := make([]string, 0, len(ctx.SharedClasses))
			for c := range ctx.SharedClasses {
				avail = append(avail, c)
			}
			sort.Strings(avail)
			errs = append(errs, verr("V13",
				"storage volume %q (class %q) uses accessMode rwx, which requires a shared-capable storage class; shared-capable classes: %s",
				s.GetName(), class, strings.Join(avail, ", ")))
		}
	}
	return errs
}

// --- V14: healthCheck http requires path+port ---

func v14(b *pb.Block) []ValidationError {
	var errs []ValidationError
	for field, probe := range map[string]*pb.HealthProbe{
		"readiness": b.GetSpec().GetNetwork().GetHealthCheck().GetReadiness(),
		"liveness":  b.GetSpec().GetNetwork().GetHealthCheck().GetLiveness(),
	} {
		if probe != nil && probe.GetType() == pb.ProbeType_PROBE_HTTP {
			if probe.GetPath() == "" || probe.GetPort() == 0 {
				errs = append(errs, verr("V14",
					"healthCheck.%s of type http requires both path and port set", field))
			}
		}
	}
	return errs
}

// --- V15: healthCheck exec requires command ---

func v15(b *pb.Block) []ValidationError {
	var errs []ValidationError
	for field, probe := range map[string]*pb.HealthProbe{
		"readiness": b.GetSpec().GetNetwork().GetHealthCheck().GetReadiness(),
		"liveness":  b.GetSpec().GetNetwork().GetHealthCheck().GetLiveness(),
	} {
		if probe != nil && probe.GetType() == pb.ProbeType_PROBE_EXEC && len(probe.GetCommand()) == 0 {
			errs = append(errs, verr("V15", "healthCheck.%s of type exec requires command set", field))
		}
	}
	return errs
}

// --- V16: antiAffinity node => replicas <= node count; message names the node count ---

func v16(b *pb.Block, ctx Context) []ValidationError {
	if b.GetSpec().GetPlacement().GetAntiAffinity() != pb.AntiAffinity_ANTI_AFFINITY_NODE || ctx.NodeCount <= 0 {
		return nil
	}
	if r := b.GetSpec().Replicas; r != nil && *r > int32(ctx.NodeCount) {
		return []ValidationError{verr("V16",
			"placement.antiAffinity node requires replicas (%d) <= node count (%d)", *r, ctx.NodeCount)}
	}
	return nil
}

// --- V17: nodeSelector keys are valid label keys ---

// validLabelKey checks the Kubernetes label-key shape:
// [prefix/]name where name is <= 63 chars of [a-zA-Z0-9] with [-_.] inside,
// and prefix (if present) is a DNS subdomain of 253-char segments.
func validLabelKey(k string) bool {
	name := k
	if i := strings.LastIndex(k, "/"); i >= 0 {
		name = k[i+1:]
		for _, seg := range strings.Split(k[:i], ".") {
			if seg == "" || len(seg) > 63 {
				return false
			}
			for j := 0; j < len(seg); j++ {
				c := seg[j]
				if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
					return false
				}
			}
			if seg[0] == '-' || seg[len(seg)-1] == '-' {
				return false
			}
		}
	}
	if name == "" || len(name) > 63 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	first, last := name[0], name[len(name)-1]
	if !(first >= 'a' && first <= 'z' || first >= 'A' && first <= 'Z' || first >= '0' && first <= '9') ||
		!(last >= 'a' && last <= 'z' || last >= 'A' && last <= 'Z' || last >= '0' && last <= '9') {
		return false
	}
	return true
}

func v17(b *pb.Block) []ValidationError {
	var errs []ValidationError
	for key := range b.GetSpec().GetPlacement().GetNodeSelector() {
		if !validLabelKey(key) {
			errs = append(errs, verr("V17", "placement.nodeSelector key %q is not a valid label key", key))
		}
	}
	return errs
}

// --- V18: requiredCapabilities are known capability strings; message names the known list ---

func v18(b *pb.Block, ctx Context) []ValidationError {
	if len(ctx.KnownCapabilities) == 0 {
		return nil
	}
	known := append([]string(nil), ctx.KnownCapabilities...)
	sort.Strings(known)
	var errs []ValidationError
	for _, c := range b.GetSpec().GetPlacement().GetRequiredCapabilities() {
		found := false
		for _, k := range known {
			if k == c {
				found = true
				break
			}
		}
		if !found {
			errs = append(errs, verr("V18",
				"placement.requiredCapabilities: unknown capability %q; known capabilities: %s", c, strings.Join(known, ", ")))
		}
	}
	return errs
}

// --- V19: config validates against the block type's schema; message names the failing field path ---

func v19(b *pb.Block, ctx Context) []ValidationError {
	if ctx.Catalog == nil {
		return nil
	}
	var errs []ValidationError
	for _, path := range ctx.Catalog.ValidateConfig(b.GetSpec().GetType(), b.GetSpec().GetConfig()) {
		errs = append(errs, verr("V19",
			"spec.config field %q does not validate against the schema of type %q", path, b.GetSpec().GetType()))
	}
	return errs
}

// --- V20: referenced secrets exist; message names the missing secret ---

func v20(b *pb.Block, ctx Context) []ValidationError {
	if ctx.SecretsExist == nil {
		return nil // TODO(Phase 06): real secrets store; nil is permissive by contract
	}
	names := make([]string, 0, len(b.GetSpec().GetSecrets()))
	for _, s := range b.GetSpec().GetSecrets() {
		names = append(names, s.GetName())
	}
	var errs []ValidationError
	for _, missing := range ctx.SecretsExist(names) {
		errs = append(errs, verr("V20", "referenced secret %q does not exist", missing))
	}
	return errs
}

// --- V21: cron schedule parses ---

func v21(b *pb.Block) []ValidationError {
	bak := b.GetSpec().GetBackup()
	if !bak.GetEnabled() || bak.GetSchedule() == "" {
		return nil
	}
	if _, err := cron.ParseStandard(bak.GetSchedule()); err != nil {
		return []ValidationError{verr("V21",
			"backup.schedule %q is not a valid cron schedule: %v", bak.GetSchedule(), err)}
	}
	return nil
}

// --- V22: maxUnavailable + maxSurge >= 1 ---

func v22(b *pb.Block) []ValidationError {
	upd := b.GetSpec().GetStrategy().GetUpdate()
	if upd == nil || upd.GetMode() != pb.UpdateMode_UPDATE_MODE_ROLLING {
		return nil
	}
	if upd.GetMaxUnavailable() < 0 || upd.GetMaxSurge() < 0 {
		return []ValidationError{verr("V22",
			"update strategy maxUnavailable (%d) and maxSurge (%d) must be >= 0", upd.GetMaxUnavailable(), upd.GetMaxSurge())}
	}
	if upd.GetMaxUnavailable()+upd.GetMaxSurge() < 1 {
		return []ValidationError{verr("V22",
			"rolling update requires maxUnavailable + maxSurge >= 1 (got %d + %d = 0)",
			upd.GetMaxUnavailable(), upd.GetMaxSurge())}
	}
	return nil
}

// --- V23: devices requested exist somewhere in the cluster; message names what's available ---

func v23(b *pb.Block, ctx Context) []ValidationError {
	devs := b.GetSpec().GetResources().GetDevices()
	if len(devs) == 0 {
		return nil
	}
	var avail []string
	for typ, count := range ctx.Devices {
		avail = append(avail, fmt.Sprintf("%s (%d)", typ, count))
	}
	sort.Strings(avail)
	availStr := strings.Join(avail, ", ")
	var errs []ValidationError
	for _, d := range devs {
		if have, ok := ctx.Devices[d.GetType()]; !ok || have <= 0 {
			errs = append(errs, verr("V23",
				"device type %q (count %d) not available in the cluster; available devices: %s", d.GetType(), d.GetCount(), availStr))
			continue
		}
		if d.GetVram() != "" {
			if _, err := quantity.ParseBytes(d.GetVram()); err != nil {
				errs = append(errs, verr("V23",
					"device %q: invalid vram quantity: %v", d.GetType(), err))
			}
		}
	}
	return errs
}

// --- V24: no cyclic dependsOn between blocks; message names the cycle ---

func v24(b *pb.Block, ctx Context) []ValidationError {
	name := b.GetMetadata().GetName()
	graph := map[string][]string{}
	for n, deps := range ctx.DependsOn {
		graph[n] = deps
	}
	graph[name] = b.GetSpec().GetDependsOn()

	// DFS from the incoming block, looking for a path back to it.
	var path []string
	onPath := map[string]bool{}
	var cycle []string
	var visit func(n string)
	visit = func(n string) {
		if cycle != nil {
			return
		}
		if n == name && len(path) > 0 {
			// Report the cycle starting and ending at the incoming block:
			// "web -> A -> web" (path holds the chain between them).
			cycle = append([]string{name}, path...)
			cycle = append(cycle, name)
			return
		}
		if onPath[n] {
			return // cycle exists but does not involve the incoming block
		}
		onPath[n] = true
		path = append(path, n)
		for _, d := range graph[n] {
			visit(d)
		}
		path = path[:len(path)-1]
		onPath[n] = false
	}
	for _, d := range b.GetSpec().GetDependsOn() {
		visit(d)
	}
	if cycle != nil {
		return []ValidationError{verr("V24",
			"cyclic dependsOn: %s", strings.Join(cycle, " -> "))}
	}
	return nil
}
