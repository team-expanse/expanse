package wire

// Production wiring adapters for the block stack (Phase 04 T20.5a).
// The controller (T06–T17) and the block service (T08, T14, T18) are
// pure components; this package connects them to live cluster state:
//
//   - Nodes: the scheduler's cluster view, built from the store's node
//     status keys plus realized placements (capacity accounting).
//
// Known Phase 05 seams (explicit, not silent gaps):
//   - Node capacity comes from DefaultCapacity, not live inventory —
//     inventory propagation across nodes is Phase 05 node-mesh work.
//   - The OvercommitConfig reads /config/scheduler when present.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/expanse/expanse/internal/cluster/join"
	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/quantity"
	"github.com/expanse/expanse/internal/scheduler"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
)

// DefaultCapacity is the conservative per-node capacity used until
// inventory propagation lands (Phase 05). Values match the controller
// test fixtures.
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
// healthy ("idle"), capacity = DefaultCapacity minus the requests of
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
			if strings.HasSuffix(string(e.Key), "/status") {
				continue // status root, not a block record
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
		views := make([]scheduler.NodeView, 0, len(ids))
		for _, id := range ids {
			ready, err := nodeReady(ctx, st, id)
			if err != nil {
				return nil, scheduler.OvercommitConfig{}, err
			}
			// The §4.8 failure detector's state gates placement: an
			// unreachable/failed (or cordoned) node is not a candidate,
			// regardless of its (possibly stale) status leaf. The
			// cordon flag travels separately — daemonsets ignore it
			// (§4.4), so they can distinguish "cordoned but healthy"
			// from "unreachable".
			var cordoned bool
			if ready {
				var placeable bool
				placeable, cordoned, err = nodePlaceable(ctx, st, id)
				if err != nil {
					return nil, scheduler.OvercommitConfig{}, err
				}
				ready = placeable
			}
			u := used[id]
			views = append(views, scheduler.NodeView{
				ID:       id,
				Ready:    ready,
				Cordoned: cordoned,
				FreeCPU:  quantity.CPU{Milli: DefaultCapacity.CPU.Milli - u.cpu},
				FreeMem:  quantity.Bytes{N: DefaultCapacity.Mem.N - u.mem},
				FreeDisk: quantity.Bytes{N: DefaultCapacity.Disk.N - u.dsk},
			})
		}
		return views, cfg, nil
	}
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

// nodeReady reports whether the node's status says healthy. The agent
// writes "idle" (or "degraded"/"error"); a missing record is not ready.
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
	// Health-reporter form: "health=<overall> [degraded=true ...]" —
	// degraded nodes are not placement candidates (§4.10.3).
	for _, tok := range strings.Fields(v) {
		if strings.HasPrefix(tok, "health=") {
			return tok[len("health="):] == "healthy", nil
		}
	}
	return false, nil
}

// nodePlaceable reads the node record's §4.8 detector state (written by
// nodelc.Monitor: state "" = up, "unreachable" past 15 s silent,
// "failed" past 5 min) and cordon flag; nodelc.placeable semantics.
func nodePlaceable(ctx context.Context, st storeReader, id string) (placeable bool, cordoned bool, err error) {
	e, err := st.Get(ctx, store.Key("/nodes/"+id))
	if err != nil {
		return false, false, errors.Wrap(err, errors.KindInternal, "wire.nodePlaceable", "node record")
	}
	var r join.NodeRecord
	if err := json.Unmarshal(e.Value, &r); err != nil {
		return false, false, errors.Wrap(err, errors.KindInternal, "wire.nodePlaceable", "unmarshal node record")
	}
	cordoned = r.Cordoned
	if r.Role == "witness" || r.Cordoned {
		return false, cordoned, nil
	}
	return r.State != "unreachable" && r.State != "failed", cordoned, nil
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
