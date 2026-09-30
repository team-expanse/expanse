// Package scheduler implements the two-stage placement algorithm from
// PHASE04.md §4: filter (hard predicates P1–P12) then score (S1–S6, T07).
//
// Determinism requirement (§4.1): given identical cluster state and block
// spec, Filter, Score and Schedule must produce identical results — no rand,
// no map iteration order, no wall clock anywhere in this package.
package scheduler

import (
	"fmt"
	"sort"
	"strings"

	"github.com/expanse/expanse/internal/quantity"
	expstorage "github.com/expanse/expanse/internal/storage"
	pb "github.com/expanse/expanse/proto"
)

// NodeView is the scheduler's view of one node.
type NodeView struct {
	ID           string
	Ready        bool
	Cordoned     bool
	Witness      bool
	FreeCPU      quantity.CPU
	FreeMem      quantity.Bytes
	FreeDisk     quantity.Bytes
	CapacityCPU  quantity.CPU // 0 = unknown (S1 neutral)
	Capabilities []string
	Labels       map[string]string
	Devices      map[string]int32 // device type -> free count
	Taints       []string
	Arch         string
	// Volumes lists volume names whose data is realized locally (S3),
	// regardless of replica health.
	Volumes []string
	// HealthyVolumes lists volume names for which this node holds a
	// *healthy* DRBD replica — the subset P12 requires for a SINGLETON
	// block with bound storage, since a candidate that cannot actually
	// promote is not a candidate at all (PHASE-03-TASKS.md D2).
	HealthyVolumes []string
	// RealizedTypes lists block type IDs ("category/name") whose nix
	// closure is already on the node (S4).
	RealizedTypes []string
	// RecentFailures counts recent node failures (S6; fewer is better).
	RecentFailures int
}

// ReplicaRequest describes one replica of one block to place.
type ReplicaRequest struct {
	Block        *pb.Block
	ReplicaIndex int
	// Resources defaults to Block.Spec.Resources when nil.
	Resources *pb.Resources
	// Placement defaults to Block.Spec.Placement when nil.
	Placement *pb.Placement
	// ExistingPlacements lists node IDs already hosting other replicas of
	// this block (anti-affinity scope).
	ExistingPlacements []string
	// Arches lists the block type's supported architectures; empty means any.
	Arches []string
}

// OvercommitConfig mirrors cluster.scheduling (§4.2).
type OvercommitConfig struct {
	CPUOvercommitRatio float64
	// Memory is NEVER overcommitted (D4.2): any ratio > 1 is clamped to 1.0.
	MemoryOvercommitRatio float64
	ReservedCPU           quantity.CPU
	ReservedMemory        quantity.Bytes
}

// Stable predicate codes, used as PendingReason codes (§4.3).
const (
	CodeNotReady           = "NotReady"
	CodeWitness            = "Witness"
	CodeInsufficientCPU    = "InsufficientCPU"
	CodeInsufficientMemory = "InsufficientMemory"
	CodeInsufficientDisk   = "InsufficientDisk"
	CodeMissingCapability  = "MissingCapability"
	CodeNodeSelectorMatch  = "NodeSelectorMismatch"
	CodeAntiAffinity       = "AntiAffinityConflict"
	CodeDeviceUnavailable  = "DeviceUnavailable"
	CodeTaintNotTolerated  = "TaintNotTolerated"
	CodeArchMismatch       = "ArchMismatch"
	CodeInvalidRequests    = "InvalidRequests"
	CodeVolumeNotLocal     = "VolumeNotLocal"
)

// Filter runs stage 1: hard predicates P1–P11 in spec order. Each rejected
// node gets a specific reason "Code: detail" (never a generic "filtered
// out"); accepted nodes are returned as candidates.
func Filter(nodes []NodeView, req ReplicaRequest, cfg OvercommitConfig) (candidates []NodeView, reasons map[string]string) {
	candidates = []NodeView{}
	reasons = map[string]string{}

	cpu, mem, disk, deviceReqs, rerr := requestQuantities(req)
	if rerr != "" {
		for _, n := range nodes {
			reasons[n.ID] = CodeInvalidRequests + ": " + rerr
		}
		return candidates, reasons
	}
	placement := req.Placement
	if placement == nil {
		placement = req.Block.GetSpec().GetPlacement()
	}
	caps := placement.GetRequiredCapabilities()
	selector := placement.GetNodeSelector()
	tolerations := toleratedSet(placement.GetTolerations())
	existing := existingSet(req.ExistingPlacements)
	anti := placement.GetAntiAffinity()

	for _, n := range nodes {
		// P1 — Ready and not cordoned.
		if !n.Ready || n.Cordoned {
			reject(reasons, n.ID, CodeNotReady,
				fmt.Sprintf("node not schedulable (ready=%t, cordoned=%t)", n.Ready, n.Cordoned))
			continue
		}
		// P2 — not a witness.
		if n.Witness {
			reject(reasons, n.ID, CodeWitness, "node is a witness")
			continue
		}
		// P3 — free cpu >= requests.cpu after overcommit ratio.
		if availCPU(n, cfg).LT(cpu) {
			reject(reasons, n.ID, CodeInsufficientCPU,
				fmt.Sprintf("needs %s cpu, %s available after reserve+overcommit", cpu, availCPU(n, cfg)))
			continue
		}
		// P4 — free memory >= requests.memory; never overcommitted (D4.2).
		if availMem(n, cfg).LT(mem) {
			reject(reasons, n.ID, CodeInsufficientMemory,
				fmt.Sprintf("needs %s memory, %s available (memory is never overcommitted)", mem, availMem(n, cfg)))
			continue
		}
		// P5 — free disk >= storage requests.
		if n.FreeDisk.LT(disk) {
			reject(reasons, n.ID, CodeInsufficientDisk,
				fmt.Sprintf("needs %s disk for volumes, %s free", disk, n.FreeDisk))
			continue
		}
		// P6 — all requiredCapabilities.
		if missing := missingStrings(caps, n.Capabilities); len(missing) > 0 {
			reject(reasons, n.ID, CodeMissingCapability,
				fmt.Sprintf("missing capabilities: %s", strings.Join(missing, ", ")))
			continue
		}
		// P7 — nodeSelector matches labels.
		if !matchesSelector(selector, n.Labels) {
			reject(reasons, n.ID, CodeNodeSelectorMatch,
				fmt.Sprintf("nodeSelector %s does not match labels %s", formatMap(selector), formatMap(n.Labels)))
			continue
		}
		// P8 — anti-affinity: no other replica of this block here.
		if anti == pb.AntiAffinity_ANTI_AFFINITY_NODE && existing[n.ID] {
			reject(reasons, n.ID, CodeAntiAffinity,
				fmt.Sprintf("node already hosts a replica of %s/%s (antiAffinity=node)",
					req.Block.GetMetadata().GetNamespace(), req.Block.GetMetadata().GetName()))
			continue
		}
		// P9 — requested devices free.
		if short := missingDevices(deviceReqs, n.Devices); len(short) > 0 {
			reject(reasons, n.ID, CodeDeviceUnavailable, "missing devices: "+short)
			continue
		}
		// P10 — all taints tolerated.
		if untolerated := untoleratedTaints(n.Taints, tolerations); len(untolerated) > 0 {
			reject(reasons, n.ID, CodeTaintNotTolerated,
				fmt.Sprintf("taints not tolerated: %s", strings.Join(untolerated, ", ")))
			continue
		}
		// P11 — architecture supported by the block.
		if len(req.Arches) > 0 && !contains(req.Arches, n.Arch) {
			reject(reasons, n.ID, CodeArchMismatch,
				fmt.Sprintf("arch %q not in supported arches %s", n.Arch, strings.Join(req.Arches, ", ")))
			continue
		}
		// P12 — a SINGLETON or DAEMONSET block with bound storage may
		// only place on a node that already holds a healthy replica of
		// every bound volume: SINGLETON's active/passive failover
		// (PHASE-03-TASKS.md D2) promotes wherever the block runs, and
		// DAEMONSET (PHASE-04-TASKS.md D2) runs one replica per candidate
		// node, so either way a node that cannot host a local replica has
		// nothing to serve, not merely a lower score.
		kind := req.Block.GetSpec().GetStrategy().GetKind()
		if kind == pb.StrategyKind_SINGLETON || kind == pb.StrategyKind_DAEMONSET {
			if missing := MissingVolumes(req.Block, n.HealthyVolumes); len(missing) > 0 {
				reject(reasons, n.ID, CodeVolumeNotLocal,
					fmt.Sprintf("missing a healthy replica of bound volume(s): %s", strings.Join(missing, ", ")))
				continue
			}
		}
		candidates = append(candidates, n)
	}
	return candidates, reasons
}

func reject(reasons map[string]string, id, code, detail string) {
	reasons[id] = code + ": " + detail
}

// availableCPU: (free - reserved) * cpuOvercommitRatio.
func availCPU(n NodeView, cfg OvercommitConfig) quantity.CPU {
	m := n.FreeCPU.Milli - cfg.ReservedCPU.Milli
	if m < 0 {
		m = 0
	}
	if cfg.CPUOvercommitRatio > 0 {
		m = int64(float64(m) * cfg.CPUOvercommitRatio)
	}
	return quantity.CPU{Milli: m}
}

// availMem: (free - reserved) * ratio, with the ratio clamped to <= 1 —
// memory is NEVER overcommitted, regardless of what a caller passes (D4.2).
func availMem(n NodeView, cfg OvercommitConfig) quantity.Bytes {
	r := cfg.MemoryOvercommitRatio
	if r <= 0 || r > 1 {
		r = 1
	}
	b := int64(float64(n.FreeMem.N)*r) - cfg.ReservedMemory.N
	if b < 0 {
		b = 0
	}
	return quantity.Bytes{N: b}
}

// Reserve charges req's requests to nodeID in nodes, so later placements in the same pass see the load.
func Reserve(nodes []NodeView, nodeID string, req ReplicaRequest) {
	cpu, mem, disk, _, errMsg := requestQuantities(req)
	if errMsg != "" {
		return
	}
	for i := range nodes {
		if nodes[i].ID == nodeID {
			nodes[i].FreeCPU.Milli -= cpu.Milli
			nodes[i].FreeMem.N -= mem.N
			nodes[i].FreeDisk.N -= disk.N
		}
	}
}

// requestQuantities parses the request quantities once; deviceReqs keeps
// insertion order for deterministic messages. An unparseable quantity (V9's
// job to reject) fails every node with InvalidRequests rather than being
// silently treated as zero.
func requestQuantities(req ReplicaRequest) (cpu quantity.CPU, mem, disk quantity.Bytes, devices []*pb.Device, errMsg string) {
	res := req.Resources
	if res == nil {
		res = req.Block.GetSpec().GetResources()
	}
	if res != nil {
		if s := res.GetRequests().GetCpu(); s != "" {
			var err error
			if cpu, err = quantity.ParseCPU(s); err != nil {
				return cpu, mem, disk, nil, err.Error()
			}
		}
		if s := res.GetRequests().GetMemory(); s != "" {
			var err error
			if mem, err = quantity.ParseBytes(s); err != nil {
				return cpu, mem, disk, nil, err.Error()
			}
		}
		devices = res.GetDevices()
	}
	for _, st := range req.Block.GetSpec().GetStorage() {
		s := st.GetSize()
		if s == "" {
			continue
		}
		sz, err := quantity.ParseBytes(s)
		if err != nil {
			return cpu, mem, disk, nil, err.Error()
		}
		disk.N += sz.N
	}
	return cpu, mem, disk, devices, ""
}

func toleratedSet(tol []string) map[string]bool {
	m := make(map[string]bool, len(tol))
	for _, t := range tol {
		m[t] = true
	}
	return m
}

func existingSet(ids []string) map[string]bool {
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func missingStrings(required, have []string) []string {
	var missing []string
	for _, c := range required {
		if !contains(have, c) {
			missing = append(missing, c)
		}
	}
	return missing
}

// MissingVolumes returns the bound storage entries (P12, and
// PHASE-04-TASKS.md D2's DAEMONSET generalization) with no matching entry
// in have (a node's healthy-volume set) — matched by each volume's
// auto-provisioned composite name (expstorage.BlockVolumeName), the same
// name reconcileBlocks creates it under and bridge.go looks it up by, not
// the storage entry's own raw name. Exported so the DAEMONSET placement
// path (internal/blocks/controller) can apply the identical rule without
// a second, potentially-diverging implementation.
func MissingVolumes(blk *pb.Block, have []string) []string {
	ns, name := blk.GetMetadata().GetNamespace(), blk.GetMetadata().GetName()
	var missing []string
	for _, s := range blk.GetSpec().GetStorage() {
		if vname := expstorage.BlockVolumeName(ns, name, s.GetName()); !contains(have, vname) {
			missing = append(missing, s.GetName())
		}
	}
	return missing
}

func missingDevices(reqs []*pb.Device, free map[string]int32) string {
	var short []string
	for _, d := range reqs {
		if free[d.GetType()] < d.GetCount() {
			short = append(short, fmt.Sprintf("%s (need %d, have %d)",
				d.GetType(), d.GetCount(), free[d.GetType()]))
		}
	}
	return strings.Join(short, ", ")
}

func untoleratedTaints(taints []string, tolerated map[string]bool) []string {
	var out []string
	for _, t := range taints {
		if !tolerated[t] {
			out = append(out, t)
		}
	}
	return out
}

func matchesSelector(selector, labels map[string]string) bool {
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

func formatMap(m map[string]string) string {
	if len(m) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// BuildPendingReason turns per-node rejection strings ("Code: detail") into
// the §4.3 PendingReason: the most common code wins Code/Message (ties break
// alphabetically), and every node's full reason is kept in PerNode.
func BuildPendingReason(reasons map[string]string) *pb.PendingReason {
	if len(reasons) == 0 {
		return nil
	}
	counts := map[string]int{}
	byCode := map[string]string{} // code -> first (sorted) full reason
	ids := make([]string, 0, len(reasons))
	for id := range reasons {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		code, _, _ := strings.Cut(reasons[id], ": ")
		counts[code]++
		if _, ok := byCode[code]; !ok {
			byCode[code] = reasons[id]
		}
	}
	codes := make([]string, 0, len(counts))
	for c := range counts {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	best := codes[0]
	for _, c := range codes {
		if counts[c] > counts[best] {
			best = c
		}
	}
	perNode := make(map[string]string, len(reasons))
	for id, r := range reasons {
		perNode[id] = strings.ToLower(strings.SplitN(r, ": ", 2)[0])
	}
	return &pb.PendingReason{
		Code:    best,
		Message: byCode[best],
		PerNode: perNode,
	}
}
