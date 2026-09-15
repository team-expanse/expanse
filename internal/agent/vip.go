package agent

// VIP management (§4.2): the agent scans block specs for ports with
// expose=VIP, allocates one VIP per block (stable, store-recorded),
// and runs a vip.Holder loop per VIP on every non-witness node. The
// holder acquires the singleton lease only while this node hosts a
// ready replica, announces the address, and serves a TCP splice to
// the local replica (T10–T12 replace the splice with real LB).

import (
	"context"
	"fmt"
	"io"
	"net/netip"
	"time"

	"github.com/expanse/expanse/internal/cluster/lease"
	"github.com/expanse/expanse/internal/network/vip"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
)

// vipScanInterval is the block/VIP re-scan cadence (also the holder
// acquisition retry rate upper bound; the holder itself retries every
// vip.AcquireRetry internally).
const vipScanInterval = 2 * time.Second

type holderRun struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func (a *Agent) initVIP() error {
	pool, err := vip.ParseExternalPool(a.cfg.ExternalVIPPool)
	if err != nil {
		return fmt.Errorf("external VIP pool: %w", err)
	}
	a.extPool = pool
	a.holders = map[string]*holderRun{}
	a.vipLeases = lease.NewManager(a.store, a.cfg.NodeID)
	// Pre-resolve the announce interface once ("auto" → default route
	// device); re-resolving per pass would fight with the mesh.
	if a.cfg.ExternalInterface != "" && a.cfg.ExternalInterface != "auto" {
		a.extIface = a.cfg.ExternalInterface
	}
	return nil
}

func (a *Agent) vipLoop(ctx context.Context) {
	if err := a.initVIP(); err != nil {
		a.logger.Error("vip disabled", "err", err)
		return
	}
	tick := time.NewTicker(vipScanInterval)
	defer tick.Stop()
	for {
		a.vipPass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// vipBlock is one desired VIP extracted from a block spec.
type vipBlock struct {
	prefix      netip.Prefix
	scope       vip.Scope
	exposedPort int32 // port the VIP listens on
	targetPort  int32 // local replica port the VIP forwards to
	blockKey    string
}

func (a *Agent) vipPass(ctx context.Context) {
	desired, cands := a.scanBlocks(ctx)

	a.vipMu.Lock()
	defer a.vipMu.Unlock()
	a.vipCands = cands

	// Stop holders whose VIP disappeared (block deleted, port changed).
	for key, hr := range a.holders {
		if _, ok := desired[key]; !ok {
			hr.cancel()
			<-hr.done
			delete(a.holders, key)
		}
	}
	// Start holders for new VIPs.
	for key, vb := range desired {
		if _, ok := a.holders[key]; ok {
			continue
		}
		hr := a.startHolder(ctx, key, vb)
		if hr != nil {
			a.holders[key] = hr
		}
	}
}

// scanBlocks lists /blocks/, extracts VIP-exposed ports, allocates
// their addresses, and collects per-VIP ready-replica candidates.
func (a *Agent) scanBlocks(ctx context.Context) (map[string]vipBlock, map[string][]vip.Candidate) {
	desired := map[string]vipBlock{}
	cands := map[string][]vip.Candidate{}

	ents, err := a.store.List(ctx, store.Key("/blocks/"))
	if err != nil {
		a.logger.Error("vip scan: list blocks", "err", err)
		return desired, cands
	}
	for _, e := range ents {
		k := string(e.Key)
		if len(k) > 7 && k[len(k)-7:] == "/status" {
			continue
		}
		var b pb.Block
		if err := proto.Unmarshal(e.Value, &b); err != nil {
			continue
		}
		// The controller stores block and status as separate keys;
		// merge so placements (readiness candidates) are visible.
		if se, err := a.store.Get(ctx, store.Key(k+"/status")); err == nil {
			var st pb.BlockStatus
			if err := proto.Unmarshal(se.Value, &st); err == nil {
				b.Status = &st
			}
		}
		port := firstVIPPort(&b)
		if port == nil {
			continue
		}
		blockRef := k[len("/blocks/"):]
		scope := vip.ScopeInternal
		pool := vip.InternalPool()
		if len(a.extPool) > 0 {
			scope = vip.ScopeExternal
			pool = a.extPool
		}
		pfx, err := vip.Allocate(ctx, a.store, blockRef, scope, pool)
		if err != nil {
			a.logger.Warn("vip allocation failed", "block", blockRef, "err", err)
			continue
		}
		target := port.GetTargetPort()
		if target == 0 {
			target = port.GetPort()
		}
		key := pfx.String()
		desired[key] = vipBlock{
			prefix: pfx, scope: scope,
			exposedPort: port.GetPort(), targetPort: target,
			blockKey: k,
		}
		cands[key] = readyCandidates(&b)
	}
	return desired, cands
}

func firstVIPPort(b *pb.Block) *pb.Port {
	for _, p := range b.GetSpec().GetNetwork().GetPorts() {
		if p.GetExpose() == pb.Expose_EXPOSE_VIP {
			return p
		}
	}
	return nil
}

// readyCandidates maps placements to §4.2 preference candidates: a
// placement counts while its phase is RUNNING. Liveness is NOT part of
// this gate: a dead node's failure is proven by its lease expiring
// (renewal every TTL/3), not by placement bookkeeping, and the holder
// takes over the moment the fence is gone (see Holder.Run).
func readyCandidates(b *pb.Block) []vip.Candidate {
	byNode := map[string]int{}
	for _, pl := range b.GetStatus().GetPlacements() {
		if pl.GetPhase() != pb.Phase_RUNNING {
			continue
		}
		byNode[pl.GetNodeId()]++
	}
	out := make([]vip.Candidate, 0, len(byNode))
	for node, n := range byNode {
		out = append(out, vip.Candidate{NodeID: node, ReadyReplicas: n})
	}
	return out
}

func (a *Agent) startHolder(ctx context.Context, key string, vb vipBlock) *holderRun {
	seams, err := a.vipSeams()
	if err != nil {
		a.logger.Warn("vip seams unavailable", "vip", key, "err", err)
		return nil
	}
	proxy := &vip.TCPProxy{Target: fmt.Sprintf("127.0.0.1:%d", vb.targetPort)}
	seams.Listen = func(p netip.Prefix) (io.Closer, error) {
		// Bind VIP:exposedPort and splice to the local replica.
		return proxy.ListenOn(fmt.Sprintf("%s:%d", p.Addr(), vb.exposedPort))
	}
	hc := vip.HolderConfig{
		Leases: a.leaseManager(),
		Self:   a.cfg.NodeID,
		VIP:    vb.prefix,
		Block:  vb.blockKey,
		Logger: a.logger.With("component", "vip"),
		Cands: func() []vip.Candidate {
			a.vipMu.Lock()
			defer a.vipMu.Unlock()
			return a.vipCands[key]
		},
		Seams: seams,
		OnAcquired: func(p netip.Prefix) {
			a.publishVIPHolder(p, a.cfg.NodeID)
		},
		OnLost: func(p netip.Prefix) {
			a.publishVIPHolder(p, "")
		},
	}
	h := vip.NewHolder(hc)
	hctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := h.Run(hctx); err != nil {
			a.logger.Warn("vip holder exited", "vip", key, "err", err)
		}
	}()
	return &holderRun{cancel: cancel, done: done}
}

// publishVIPHolder writes (holder != "") or clears (holder == "") the
// /network/vips/<addr>/holder record the mesh reconciler reads for
// VIP /32 AllowedIPs. Writes are unconditional Puts: only the current
// lease holder publishes, and lease TTL bounds any stale overwrite.
func (a *Agent) publishVIPHolder(p netip.Prefix, holder string) {
	key := store.Key(fmt.Sprintf("/network/vips/%s/holder", p.Addr()))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var err error
	if holder == "" {
		err = a.store.Delete(ctx, key, 0)
	} else {
		_, err = a.store.Put(ctx, key, []byte(holder))
	}
	if err != nil {
		a.logger.Warn("vip holder record update failed", "vip", p.Addr(), "err", err)
	}
}

// vipSeams builds the platform seams for VIP holdership. The netlink
// and gratuitous-ARP edges come from vip.LinuxSeams; the listener edge
// is filled in per-holder by startHolder.
func (a *Agent) vipSeams() (vip.Seams, error) {
	seams, err := vip.LinuxSeams(a.extIfaceOrAuto())
	if err != nil {
		return vip.Seams{}, err
	}
	return seams, nil
}

func (a *Agent) extIfaceOrAuto() string {
	if a.extIface != "" {
		return a.extIface
	}
	return "auto"
}

// leaseManager returns the lease manager created by initVIP (same
// store, same node ID as the blocks controller's managers).
func (a *Agent) leaseManager() *lease.Manager {
	return a.vipLeases
}
