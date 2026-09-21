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

// runVolumes starts the volume node loop and returns the function that stops it
// and waits until it has demoted every volume.
func (a *Agent) runVolumes(ctx context.Context, cancel context.CancelFunc) func() {
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.volnode.Run(ctx, volumeSyncInterval)
	}()
	return func() {
		cancel()
		<-done
	}
}

// initStorage wires the DRBD volume stack: the node loop that converges the
// replicas placed here, and the leader-side controller that plans them.
func (a *Agent) initStorage(cfg Config, st store.Store, logger *slog.Logger) error {
	marks, err := volume.NewSplitBrainMarks(filepath.Join(cfg.DataDir, splitBrainDir))
	if err != nil {
		return fmt.Errorf("split-brain markers: %w", err)
	}
	dr := drbd.New()
	mounts := mount.New(nil, dr, "")
	a.recon.Register(mounts)
	promoter := &volume.Promoter{DRBD: dr, Consumer: mounts, Log: logger}
	leases := lease.NewManager(st, cfg.NodeID)
	alloc := drbd.NewAllocator(st, drbd.DefaultMinors, drbd.DefaultPorts)
	a.volnode = &volume.Node{
		Splits: marks, Self: cfg.NodeID, St: st, Alloc: alloc, DRBD: dr, Log: logger,
		RT: &volume.Runtime{
			LVM: lvm.New(), DRBD: dr, VG: cfg.StorageVG, Pool: cfg.StoragePool, ConfigDir: cfg.DRBDConfigDir,
			SplitBrainCmd: marks.Handler(), Diverged: marks.Marked,
		},
		Lead: func(ctx context.Context, res string, opt volume.HoldOptions) error {
			return promoter.Lead(ctx, leases, volume.LeaseName(res), res, volumeLeaseTTL, opt)
		},
		Addr: a.meshAddr,
		Thin: cfg.StoragePool != "",
	}
	a.volctl = volctlc.New(volctlc.Options{
		NodeID: cfg.NodeID, St: st, Alloc: alloc, Logger: logger, LostAfter: cfg.StorageLostAfter,
		IsLeader: func() bool { return a.ctl != nil && a.ctl.store != nil && a.ctl.store.IsLeader() },
	})
	return nil
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
