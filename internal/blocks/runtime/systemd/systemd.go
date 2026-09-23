// Package systemd implements the Phase 04 block runtime on systemd
// (PHASE04.md §5.3): unit naming, systemd unit generation with cgroup
// limits + sandboxing, the NixOS module fragment, the closure cache that
// skips nix builds when the closure inputs are unchanged, and a
// reconcile.Manager for "block-replica" resources wired into the agent's
// existing reconcile loop.
//
// Key design point — templated units: each replica is an INSTANCE of a
// template unit, `expanse-block@<namespace>-<name>-<index>.service`
// (instance of expanse-block@.service, %i = "<namespace>-<name>-<index>").
// The nix closure therefore contains per-block content only, never the
// replica index: a replicas-only spec change reuses the cached closure,
// skips build AND switch, and only touches unit state (< 5 s, §5.3).
package systemd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/expanse/expanse/internal/quantity"
	pb "github.com/expanse/expanse/proto"
)

// UnitPrefix is the identifier prefix for generated units.
const UnitPrefix = "expanse-block"

// RootUnitPrefix is the identifier prefix for RunAsRoot replicas
// (nix/modules/agent.nix's expanse-block-root@.service). The real
// running unit is a fixed, static template baked in at image-build
// time — it cannot vary DynamicUser per replica the way Spec's own
// RunAsRoot field would suggest — so a workload that must run
// unsandboxed by uid (share/smb, share/nfs: PHASE-03-TASKS.md D1/D3)
// gets routed to a second, dedicated static template instead.
const RootUnitPrefix = "expanse-block-root"

// Slice is the cgroup slice all block units live in.
const Slice = "expanse-blocks.slice"

// Instance is the unit instance identifier "<namespace>-<name>-<index>".
func Instance(namespace, name string, index int) string {
	return fmt.Sprintf("%s-%s-%d", namespace, name, index)
}

// UnitName is the templated systemd unit for one replica.
func UnitName(namespace, name string, index int) string {
	return UnitPrefix + "@" + Instance(namespace, name, index) + ".service"
}

// UnitNameForSpec is UnitName, routed to RootUnitPrefix's template when
// s.RunAsRoot asks for it.
func UnitNameForSpec(s Spec) string {
	prefix := UnitPrefix
	if s.RunAsRoot {
		prefix = RootUnitPrefix
	}
	return prefix + "@" + Instance(s.Namespace, s.Name, s.Index) + ".service"
}

// JournaldIdentifier is the SYSLOG_IDENTIFIER for a replica's logs (§5.5).
func JournaldIdentifier(namespace, name string, index int) string {
	return UnitPrefix + "-" + Instance(namespace, name, index)
}

// Spec is the desired state of one block replica on one node. It is the
// unit-generation input AND the reconcile resource spec (JSON-encoded by
// the manager). Fields mirror §5.3.
type Spec struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Index     int    `json:"index"`
	// Type is the block type ID ("category/name") — resolved to the
	// package/entrypoint by the catalog (Phase 05 wiring).
	Type string   `json:"type"`
	Args []string `json:"args,omitempty"`
	// Limits drive the cgroup properties (§5.3). Requests are enforced by
	// scheduling, not the unit.
	Limits *pb.ResourcePair `json:"limits,omitempty"`
	// VolumeMounts list host paths exposed via ReadWritePaths (sandbox
	// must open them explicitly under ProtectSystem=strict).
	VolumeMounts []string `json:"volumeMounts,omitempty"`
	// BindPaths bind host directories into the unit's namespace as
	// "host[:inUnit]" entries (T14 §4.7: volume mounts).
	BindPaths []string `json:"bindPaths,omitempty"`
	// Credentials map systemd credential name -> source path, surfaced
	// with LoadCredential= (never env vars, never /nix/store files).
	Credentials map[string]string `json:"credentials,omitempty"`
	// ExtraAddressFamilies extends the default AF_UNIX AF_INET AF_INET6.
	ExtraAddressFamilies []string `json:"extraAddressFamilies,omitempty"`
	// IOWeight (0–1000); 0 emits systemd's default 100.
	IOWeight int `json:"ioWeight,omitempty"`
	// TasksMax caps tasks in the cgroup; 0 emits 512.
	TasksMax int `json:"tasksMax,omitempty"`
	// StaticUID, when > 0, pins the unit to that uid; otherwise
	// DynamicUser=yes.
	StaticUID int `json:"staticUid,omitempty"`
	// RunAsRoot disables DynamicUser without pinning a uid (root is
	// systemd's own default when User= is unset). For workloads that
	// must setuid()/setgid() at runtime (share/smb, share/nfs) — a
	// capability DynamicUser's unprivileged random uid can never hold.
	RunAsRoot bool `json:"runAsRoot,omitempty"`
}

// UnitFile renders the systemd TEMPLATE unit (expanse-block@.service):
// per-replica identity arrives via the instance string %i. Keeping the
// index out of the file is what makes a replicas-only change a cache hit.
// Output is deterministic (sorted map keys) — tests assert exact lines.
func UnitFile(s Spec) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[Unit]\n")
	fmt.Fprintf(&b, "Description=expanse block %%i\n")
	fmt.Fprintf(&b, "After=network-online.target\n")
	fmt.Fprintf(&b, "StartLimitIntervalSec=60\n")
	fmt.Fprintf(&b, "StartLimitBurst=3\n")
	fmt.Fprintf(&b, "\n[Service]\n")
	fmt.Fprintf(&b, "Slice=%s\n", Slice)
	// Tags every log line with JournaldIdentifier's exact format (§5.5):
	// without this, journald's default SYSLOG_IDENTIFIER is the started
	// binary's own name, not the unit's, and StreamLogs's
	// --syslog-identifier query would match nothing.
	fmt.Fprintf(&b, "SyslogIdentifier=%s-%%i\n", UnitPrefix)
	// Restart policy (§5.3).
	fmt.Fprintf(&b, "Restart=on-failure\n")
	fmt.Fprintf(&b, "RestartSec=5s\n")
	// cgroup limits from resources.limits.
	if s.Limits != nil {
		if s.Limits.Cpu != "" {
			if q, err := quantity.ParseCPU(s.Limits.Cpu); err == nil {
				fmt.Fprintf(&b, "CPUQuota=%d%%\n", q.Milli/10)
			}
		}
		if s.Limits.Memory != "" {
			if q, err := quantity.ParseBytes(s.Limits.Memory); err == nil {
				fmt.Fprintf(&b, "MemoryMax=%d\n", q.N)
				// Soft limit before hard kill (§5.3): MemoryHigh = 90%.
				fmt.Fprintf(&b, "MemoryHigh=%d\n", q.N*9/10)
			}
		}
	}
	fmt.Fprintf(&b, "IOWeight=%d\n", ioWeightOr(s.IOWeight))
	fmt.Fprintf(&b, "TasksMax=%d\n", tasksMaxOr(s.TasksMax))
	// Identity: DynamicUser by default, static uid when pinned, root
	// when RunAsRoot asks for systemd's own unset-User= default.
	switch {
	case s.RunAsRoot:
		fmt.Fprintf(&b, "DynamicUser=no\n")
	case s.StaticUID > 0:
		fmt.Fprintf(&b, "DynamicUser=no\n")
		fmt.Fprintf(&b, "User=%d\n", s.StaticUID)
	default:
		fmt.Fprintf(&b, "DynamicUser=yes\n")
	}
	// Sandboxing — ALWAYS present, every block type (§5.3).
	fmt.Fprintf(&b, "NoNewPrivileges=yes\n")
	fmt.Fprintf(&b, "PrivateTmp=yes\n")
	fmt.Fprintf(&b, "ProtectSystem=strict\n")
	fmt.Fprintf(&b, "ProtectHome=yes\n")
	if len(s.BindPaths) > 0 {
		binds := append([]string(nil), s.BindPaths...)
		sort.Strings(binds)
		fmt.Fprintf(&b, "BindPaths=%s\n", strings.Join(binds, " "))
	}
	if len(s.VolumeMounts) > 0 {
		paths := append([]string(nil), s.VolumeMounts...)
		sort.Strings(paths)
		fmt.Fprintf(&b, "ReadWritePaths=%s\n", strings.Join(paths, " "))
	}
	fams := append([]string{"AF_UNIX", "AF_INET", "AF_INET6"}, s.ExtraAddressFamilies...)
	fmt.Fprintf(&b, "RestrictAddressFamilies=%s\n", strings.Join(fams, " "))
	fmt.Fprintf(&b, "SystemCallFilter=@system-service\n")
	// Credentials: LoadCredential=<name>:<source path> (§5.3).
	for _, name := range sortedKeys(s.Credentials) {
		fmt.Fprintf(&b, "LoadCredential=%s:%s\n", name, s.Credentials[name])
	}
	// Exec: %i splits into namespace-name-index at unit start.
	fmt.Fprintf(&b, "ExecStart=%s\n", execLine(s))
	return b.String()
}

func ioWeightOr(w int) int {
	if w == 0 {
		return 100
	}
	return w
}

func tasksMaxOr(t int) int {
	if t == 0 {
		return 512
	}
	return t
}

func execLine(s Spec) string {
	parts := append([]string{
		"/run/current-system/sw/bin/expanse-block-run",
		s.Type, "%i",
	}, s.Args...)
	return strings.Join(quoted(parts), " ")
}

func quoted(parts []string) []string {
	out := make([]string, len(parts))
	for i, p := range parts {
		if strings.ContainsAny(p, " \t\"'\\") {
			out[i] = fmt.Sprintf("%q", p)
		} else {
			out[i] = p
		}
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// ModuleFragment renders the NixOS module fragment declaring the template
// unit's file, for the nix driver to build+switch. It calls the block
// catalog's per-type module (T05 contract: {pkgs, name, port, body}) —
// the fragment itself is pure text so tests can assert it.
func ModuleFragment(s Spec) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# expanse: block %s/%s replica template\n", s.Namespace, s.Name)
	fmt.Fprintf(&b, "systemd.packages = [ (pkgs.writeTextDir \"etc/systemd/system/%s\" ''\n",
		UnitPrefix+"@"+".service")
	fmt.Fprintf(&b, "%s'') ];\n", UnitFile(s))
	return b.String()
}
