package agent

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"path/filepath"
	"time"

	"github.com/expanse/expanse/internal/cluster/lease"
	networkmesh "github.com/expanse/expanse/internal/network/mesh"
	volctlc "github.com/expanse/expanse/internal/storage/controller"
	"github.com/expanse/expanse/internal/storage/drbd"
	"github.com/expanse/expanse/internal/storage/lvm"
	"github.com/expanse/expanse/internal/storage/mount"
	"github.com/expanse/expanse/internal/storage/volume"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
	pbproto "google.golang.org/protobuf/proto"
)

const (
	volumeLeaseTTL       = 10 * time.Second
	volumeSyncInterval   = 2 * time.Second
	defaultDRBDConfigDir = "/etc/drbd.d"
	// splitBrainDir, under the data directory, is where the kernel's split-brain handler leaves its marks.
	splitBrainDir = "split-brain"
)

// volumeRunner is the node loop that converges this machine's volume replicas;
// *volume.Node implements it. Run returns only after every volume is demoted.
type volumeRunner interface {
	Run(ctx context.Context, interval time.Duration)
}

// startVolumes runs the volume node loop until stopVolumes; it is a no-op once
// volumes run or the agent is shutting down. build makes the node.
func (a *Agent) startVolumes(ctx context.Context, build func() volumeRunner) {
	a.volMu.Lock()
	defer a.volMu.Unlock()
	if a.volStop != nil || a.volClosed {
		return
	}
	node := build()
	// Detached from ctx: only stopVolumes ends it, as a shutdown, so busy volumes stay primary.
	vctx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		node.Run(vctx, volumeSyncInterval)
	}()
	a.volStop = func() {
		cancel(volume.ErrShutdown)
		<-done
	}
}

// stopVolumes stops the volume node loop, waiting until it has demoted every
// volume nothing uses, and keeps it from starting again.
func (a *Agent) stopVolumes() {
	a.volMu.Lock()
	defer a.volMu.Unlock()
	a.volClosed = true
	if a.volStop != nil {
		a.volStop()
		a.volStop = nil
	}
}

// initStorage wires the leader-side controller that plans volume replicas, and
// the volume node for the VG in the node's config, if it names one. The mount
// manager registers now because the reconciler cannot take one while it runs.
func (a *Agent) initStorage(cfg Config, st store.Store, logger *slog.Logger) error {
	marks, err := volume.NewSplitBrainMarks(filepath.Join(cfg.DataDir, splitBrainDir))
	if err != nil {
		return fmt.Errorf("split-brain markers: %w", err)
	}
	a.splitMarks = marks
	a.drbd = drbd.New()
	a.mounts = mount.New(nil, a.drbd, "")
	a.recon.Register(a.mounts)
	a.drbdAlloc = drbd.NewAllocator(st, drbd.DefaultMinors, drbd.DefaultPorts)
	a.volctl = volctlc.New(volctlc.Options{
		NodeID: cfg.NodeID, St: st, Alloc: a.drbdAlloc, Logger: logger, LostAfter: cfg.StorageLostAfter,
		IsLeader: func() bool { return a.ctl != nil && a.ctl.store != nil && a.ctl.store.IsLeader() },
	})
	a.lvm = lvm.New()
	a.drbdCheck = func() error { return drbd.CheckModule(drbd.ProcPath) }
	a.newVolNode = a.volumeNode
	if cfg.StorageVG != "" {
		a.volnode = a.volumeNode(cfg.StorageVG, cfg.StoragePool)
		a.storageVG.Store(cfg.StorageVG)
	}
	return nil
}

// volumeNode builds the loop that converges the replicas placed on this node,
// in pool (thin) or straight in vg when pool is empty.
func (a *Agent) volumeNode(vg, pool string) volumeRunner {
	cfg, logger, dr := a.cfg, a.logger, a.drbd
	promoter := &volume.Promoter{DRBD: dr, Consumer: a.mounts, Log: logger}
	leases := lease.NewManager(a.store, cfg.NodeID)
	return &volume.Node{
		Splits: a.splitMarks, Self: cfg.NodeID, St: a.store, Alloc: a.drbdAlloc, DRBD: dr, Log: logger,
		RT: &volume.Runtime{
			LVM: a.lvm, DRBD: dr, VG: vg, Pool: pool, ConfigDir: cfg.DRBDConfigDir,
			SplitBrainCmd: a.splitMarks.Handler(), Diverged: a.splitMarks.Marked,
			CheckKernel: a.drbdCheck,
		},
		Lead: func(ctx context.Context, res string, opt volume.HoldOptions) error {
			return promoter.Lead(ctx, leases, volume.LeaseName(res), res, volumeLeaseTTL, opt)
		},
		Addr: a.meshAddr,
		Thin: pool != "",
	}
}

// meshAddr resolves a node to its exp0 overlay address: the .1 of the /24 in its
// published overlay prefix (§4.1's 10.42.N.1 convention).
func (a *Agent) meshAddr(nodeID string) (netip.Addr, error) {
	entry, err := a.store.Get(context.Background(), store.Key(networkmesh.PublicKeyKey(nodeID)))
	if err != nil {
		return netip.Addr{}, fmt.Errorf("no mesh record for %s: %w", nodeID, err)
	}
	var peer pb.WireGuardPeer
	if err := pbproto.Unmarshal(entry.Value, &peer); err != nil {
		return netip.Addr{}, fmt.Errorf("mesh record for %s unreadable: %w", nodeID, err)
	}
	prefix, err := netip.ParsePrefix(peer.GetOverlayPrefix())
	if err != nil {
		return netip.Addr{}, fmt.Errorf("bad overlay prefix for %s: %w", nodeID, err)
	}
	addr := prefix.Addr().As4()
	addr[3] = 1
	return netip.AddrFrom4(addr), nil
}
