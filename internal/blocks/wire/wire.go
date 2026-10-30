package wire

// Production wiring adapters for the block stack (Phase 04 T20.5a).
// The controller (T06–T17) and the block service (T08, T14, T18) are
// pure components; this package connects them to live cluster state:
//
//   - Nodes: the scheduler's cluster view, built from the store's node
//     status keys plus realized placements (capacity accounting).
//
// Notes:
//   - Node capacity is what each agent publishes (CapacityKey), with
//     DefaultCapacity for a node that has not published it.
//   - The OvercommitConfig reads /config/scheduler when present.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/expanse/expanse/internal/blocks/blockkey"

	"github.com/expanse/expanse/internal/cluster/join"
	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/quantity"
	"github.com/expanse/expanse/internal/scheduler"
	expstorage "github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
)

// DefaultCapacity is the per-node capacity used for any field a node has
// not published. Values match the controller test fixtures.
var DefaultCapacity = struct {
	CPU  quantity.CPU
	Mem  quantity.Bytes
	Disk quantity.Bytes
}{
	CPU:  quantity.CPU{Milli: 4000},
	Mem:  quantity.Bytes{N: 8 << 30},
	Disk: quantity.Bytes{N: 100 << 30},
}

// storeReader is the read half of store.Store.
type storeReader interface {
	Get(ctx context.Context, k store.Key) (*store.Entry, error)
	List(ctx context.Context, prefix store.Key) ([]*store.Entry, error)
}

// Nodes builds the scheduler cluster view from the store: one NodeView
// per node record under /nodes/<id>, Ready iff its status value reports
// healthy ("idle"), capacity = nodeCapacity minus the requests of
// active placements already on the node.
func Nodes(st storeReader) func(context.Context) ([]scheduler.NodeView, scheduler.OvercommitConfig, error) {
	return func(ctx context.Context) ([]scheduler.NodeView, scheduler.OvercommitConfig, error) {
		ids, err := nodeIDs(ctx, st)
		if err != nil {
			return nil, scheduler.OvercommitConfig{}, err
		}
		// Per-node requests from active placements (§4.2 accounting).
		type usage struct{ cpu, mem, dsk int64 }
		used := map[string]usage{}
		entries, err := st.List(ctx, "/blocks/")
		if err != nil {
			return nil, scheduler.OvercommitConfig{}, errors.Wrap(err,
				errors.KindInternal, "wire.Nodes", "list blocks")
		}
		for _, e := range entries {
			if !blockkey.IsSpec(e.Key) {
				continue // status, probe records: not a block record
			}
			var b pb.Block
			if err := proto.Unmarshal(e.Value, &b); err != nil {
				continue // unreadable block: skip, don't fail the view
			}
			cpu, mem, dsk := requestsOf(&b)
			var status pb.BlockStatus
			se, err := st.Get(ctx, store.Key(string(e.Key)+"/status"))
			if err != nil {
				continue
			}
			if err := proto.Unmarshal(se.Value, &status); err != nil {
				continue
			}
			for _, p := range status.GetPlacements() {
				if p.GetReplicaIndex() < 0 || p.GetPhase() == pb.Phase_LOST {
					continue // retired records hold no capacity
				}
				u := used[p.GetNodeId()]
				u.cpu += cpu
				u.mem += mem
				u.dsk += dsk
				used[p.GetNodeId()] = u
			}
		}

		cfg, err := overcommitConfig(ctx, st)
		if err != nil {
			return nil, scheduler.OvercommitConfig{}, err
		}
		// §4.1 S3 data locality (weight 15): which nodes hold replicas
		// of which volumes (volume NAME → replica node set), plus the
		// healthy-only subset P12 (PHASE-03-TASKS.md D2) requires.
		volNodes, healthyVolNodes, err := volumeLocality(ctx, st)
		if err != nil {
			return nil, scheduler.OvercommitConfig{}, err
		}
		views := make([]scheduler.NodeView, 0, len(ids))
		for _, id := range ids {
			local := []string{}
			for name, nodes := range volNodes {
				if nodes[id] {
					local = append(local, name)
				}
			}
			healthyLocal := []string{}
			for name, nodes := range healthyVolNodes {
				if nodes[id] {
					healthyLocal = append(healthyLocal, name)
				}
			}
			ready, err := nodeReady(ctx, st, id)
			if err != nil {
				return nil, scheduler.OvercommitConfig{}, err
			}
			// The §4.8 detector state and cordon gate placement; the cordon and
			// drain flags travel separately so "cordoned but up" is not "unreachable".
			var lc lifecycle
			if ready {
				if lc, err = nodeLifecycle(ctx, st, id); err != nil {
					return nil, scheduler.OvercommitConfig{}, err
				}
				ready = lc.placeable
			}
			u := used[id]
			c := nodeCapacity(ctx, st, id)
			views = append(views, scheduler.NodeView{
				ID:             id,
				Ready:          ready,
				Cordoned:       lc.cordoned,
				Draining:       lc.draining,
				Capabilities:   nodeCapabilities(ctx, st, id),
				Volumes:        local,
				HealthyVolumes: healthyLocal,
				CapacityCPU:    c.CPU,
				FreeCPU:        quantity.CPU{Milli: c.CPU.Milli - u.cpu},
				FreeMem:        quantity.Bytes{N: c.Mem.N - u.mem},
				FreeDisk:       quantity.Bytes{N: c.Disk.N - u.dsk},
			})
		}
		return views, cfg, nil
	}
}

// volumeLocality maps volume NAME → set of nodes holding a replica, and
// separately the subset of that set where the replica is healthy (P12).
// It reads the raw volume records directly (storeReader surface).
func volumeLocality(ctx context.Context, st storeReader) (all, healthy map[string]map[string]bool, err error) {
	all = map[string]map[string]bool{}
	healthy = map[string]map[string]bool{}
	entries, err := st.List(ctx, store.Key(expstorage.VolumePrefix))
	if err != nil {
		return nil, nil, err
	}
	// Walk unique volume IDs from /volumes/<id>/spec|status keys.
	ids := map[string]bool{}
	for _, e := range entries {
		parts := strings.SplitN(strings.TrimPrefix(string(e.Key), expstorage.VolumePrefix), "/", 2)
		if len(parts) == 2 && parts[1] == "spec" {
			ids[parts[0]] = true
		}
	}
	for id := range ids {
		se, err := st.Get(ctx, expstorage.SpecKey(id))
		if err != nil {
			continue
		}
		var spb pb.VolumeSpec
		if proto.Unmarshal(se.Value, &spb) != nil {
			continue
		}
		te, err := st.Get(ctx, expstorage.StatusKey(id))
		if err != nil {
			continue
		}
		var tpb pb.VolumeStatus
		if proto.Unmarshal(te.Value, &tpb) != nil {
			continue
		}
		spec := expstorage.SpecFromProto(&spb)
		status := expstorage.StatusFromProto(&tpb)
		nodes := map[string]bool{}
		healthyNodes := map[string]bool{}
		for _, p := range status.Placement {
			nodes[p.NodeID] = true
			if p.Healthy {
				healthyNodes[p.NodeID] = true
			}
		}
		all[spec.Name] = nodes
		healthy[spec.Name] = healthyNodes
	}
	return all, healthy, nil
}

// requestsOf sums a block's requests: cpu/mem from resources.requests,
// disk from the storage volume sizes (same sources as the scheduler).
func requestsOf(b *pb.Block) (cpu, mem, dsk int64) {
	if req := b.GetSpec().GetResources().GetRequests(); req != nil {
		if s := req.GetCpu(); s != "" {
			if q, err := quantity.ParseCPU(s); err == nil {
				cpu = q.Milli
			}
		}
		if s := req.GetMemory(); s != "" {
			if q, err := quantity.ParseBytes(s); err == nil {
				mem = q.N
			}
		}
	}
	for _, st := range b.GetSpec().GetStorage() {
		if q, err := quantity.ParseBytes(st.GetSize()); err == nil {
			dsk += q.N
		}
	}
	return cpu, mem, dsk
}

func nodeIDs(ctx context.Context, st storeReader) ([]string, error) {
	entries, err := st.List(ctx, "/nodes/")
	if err != nil {
		return nil, errors.Wrap(err, errors.KindInternal, "wire.Nodes", "list nodes")
	}
	seen := map[string]bool{}
	var ids []string
	for _, e := range entries {
		k := string(e.Key)
		// Node records are direct children; skip status/health leaves.
		rest := strings.TrimPrefix(k, "/nodes/")
		if rest == "" || strings.Contains(rest, "/") {
			continue
		}
		if !seen[rest] {
			seen[rest] = true
			ids = append(ids, rest)
		}
	}
	return ids, nil
}

// nodeReady reports whether the node's status lets it take placements: the agent's schedulable=
// verdict, or health=healthy from an agent that predates it. No quorum, or no record, is not ready.
func nodeReady(ctx context.Context, st storeReader, id string) (bool, error) {
	e, err := st.Get(ctx, store.Key("/nodes/"+id+"/status"))
	if err != nil {
		if errors.Is(err, errors.KindNotFound) {
			return false, nil
		}
		return false, errors.Wrap(err, errors.KindInternal, "wire.Nodes", "node status")
	}
	v := strings.TrimSpace(string(e.Value))
	if v == "idle" || v == "healthy" {
		return true, nil
	}
	fields := map[string]string{}
	for _, tok := range strings.Fields(v) {
		if k, val, ok := strings.Cut(tok, "="); ok {
			fields[k] = val
		}
	}
	if fields["degraded"] == "true" {
		return false, nil // §4.10.3: a node without quorum is not a placement candidate
	}
	if s, ok := fields["schedulable"]; ok {
		return s == "true", nil
	}
	return fields["health"] == "healthy", nil
}

// Capacity is a node's schedulable size.
type Capacity struct {
	CPU  quantity.CPU
	Mem  quantity.Bytes
	Disk quantity.Bytes
}

// CapacityKey is where a node's agent publishes its Capacity, as FormatCapacity renders it.
func CapacityKey(nodeID string) store.Key { return store.Key("/nodes/" + nodeID + "/capacity") }

// FormatCapacity renders "cpu=<milli> memory=<bytes> disk=<bytes>"; a zero field is left out.
func FormatCapacity(c Capacity) string {
	var parts []string
	if c.CPU.Milli > 0 {
		parts = append(parts, fmt.Sprintf("cpu=%d", c.CPU.Milli))
	}
	if c.Mem.N > 0 {
		parts = append(parts, fmt.Sprintf("memory=%d", c.Mem.N))
	}
	if c.Disk.N > 0 {
		parts = append(parts, fmt.Sprintf("disk=%d", c.Disk.N))
	}
	return strings.Join(parts, " ")
}

// nodeCapacity reads the node's published capacity; an absent or unparseable field keeps
// DefaultCapacity's, so a node from before publishing still schedules as it did.
func nodeCapacity(ctx context.Context, st storeReader, id string) Capacity {
	c := Capacity{CPU: DefaultCapacity.CPU, Mem: DefaultCapacity.Mem, Disk: DefaultCapacity.Disk}
	e, err := st.Get(ctx, CapacityKey(id))
	if err != nil {
		return c
	}
	for _, f := range strings.Fields(string(e.Value)) {
		k, v, _ := strings.Cut(f, "=")
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			continue
		}
		switch k {
		case "cpu":
			c.CPU.Milli = n
		case "memory":
			c.Mem.N = n
		case "disk":
			c.Disk.N = n
		}
	}
	return c
}

// nodeCapabilities reads the node's self-published capability list
// (internal/agent.Agent.refreshInventory), e.g. ["kvm", "aes-ni"] — the
// leader-side view's channel for another node's hardware, alongside
// nodeCapacity. A missing key (a node that hasn't published yet, or
// never will — witnesses skip inventory collection entirely) means no
// known capabilities, the same as before this was wired at all: P6
// simply finds every declared requirement missing, not a hard error.
func nodeCapabilities(ctx context.Context, st storeReader, id string) []string {
	e, err := st.Get(ctx, store.Key("/nodes/"+id+"/capabilities"))
	if err != nil {
		return nil
	}
	v := strings.TrimSpace(string(e.Value))
	if v == "" {
		return nil
	}
	return strings.Split(v, ",")
}

// lifecycle is a node record's placement state; cordoned and draining are
// reported only for a node the §4.8 detector still sees as up.
type lifecycle struct{ placeable, cordoned, draining bool }

// nodeLifecycle reads the node record's detector state (written by
// nodelc.Monitor) and cordon/drain flags; nodelc.placeable semantics.
func nodeLifecycle(ctx context.Context, st storeReader, id string) (lifecycle, error) {
	e, err := st.Get(ctx, store.Key("/nodes/"+id))
	if err != nil {
		return lifecycle{}, errors.Wrap(err, errors.KindInternal, "wire.nodeLifecycle", "node record")
	}
	var r join.NodeRecord
	if err := json.Unmarshal(e.Value, &r); err != nil {
		return lifecycle{}, errors.Wrap(err, errors.KindInternal, "wire.nodeLifecycle", "unmarshal node record")
	}
	if (r.State == "unreachable" || r.State == "failed") && !reportedSince(ctx, st, id, e.Revision) {
		return lifecycle{}, nil
	}
	return lifecycle{
		placeable: r.Role != "witness" && !r.Cordoned,
		cordoned:  r.Cordoned,
		draining:  r.Draining,
	}, nil
}

// reportedSince reports whether node id wrote its status after rev, its detector mark: it is back,
// though the detector has not cleared the mark yet.
func reportedSince(ctx context.Context, st storeReader, id string, rev store.Revision) bool {
	e, err := st.Get(ctx, store.Key("/nodes/"+id+"/status"))
	return err == nil && e.Revision > rev
}

// overcommitConfig reads /config/scheduler if present, else defaults
// (§4.2: cpu ratio 2.0, memory never overcommitted).
func overcommitConfig(ctx context.Context, st storeReader) (scheduler.OvercommitConfig, error) {
	cfg := scheduler.OvercommitConfig{CPUOvercommitRatio: 2.0, MemoryOvercommitRatio: 1.0}
	e, err := st.Get(ctx, store.Key("/config/scheduler"))
	if err != nil {
		if errors.Is(err, errors.KindNotFound) {
			return cfg, nil
		}
		return cfg, errors.Wrap(err, errors.KindInternal, "wire.Nodes", "scheduler config")
	}
	var stored struct {
		CPUOvercommitRatio    float64 `json:"cpuOvercommitRatio"`
		MemoryOvercommitRatio float64 `json:"memoryOvercommitRatio"`
		ReservedCPU           string  `json:"reservedCpu"`
		ReservedMemory        string  `json:"reservedMemory"`
	}
	if err := json.Unmarshal(e.Value, &stored); err != nil {
		return cfg, errors.New(errors.KindInvalid, "wire.Nodes",
			fmt.Sprintf("parse /config/scheduler: %v", err))
	}
	if stored.CPUOvercommitRatio > 0 {
		cfg.CPUOvercommitRatio = stored.CPUOvercommitRatio
	}
	if stored.MemoryOvercommitRatio > 0 {
		cfg.MemoryOvercommitRatio = stored.MemoryOvercommitRatio
	}
	if stored.ReservedCPU != "" {
		if q, err := quantity.ParseCPU(stored.ReservedCPU); err == nil {
			cfg.ReservedCPU = q
		}
	}
	if stored.ReservedMemory != "" {
		if q, err := quantity.ParseBytes(stored.ReservedMemory); err == nil {
			cfg.ReservedMemory = q
		}
	}
	return cfg, nil
}
