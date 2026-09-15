package mesh

import (
	"context"
	"fmt"
	"net/netip"
	"strings"

	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/network/addrplan"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/proto"
	pbproto "google.golang.org/protobuf/proto"
)

// DefaultMTU is the §4.1 default exp0 MTU (physical 1500 − 80).
const DefaultMTU = 1420

// InterfaceName is the mesh interface (§4.1).
const InterfaceName = "exp0"

// Config configures the mesh reconciler.
type Config struct {
	// Interface defaults to exp0.
	Interface string
	// MTUOverride, when > 0, replaces the computed MTU.
	MTUOverride int
	// PhysicalMTU, when > 0, skips auto-detection (used by the Linux
	// controller and tests). 0 → detect, default 1500.
	PhysicalMTU int
	// ListenPort is the WireGuard UDP port (51820).
	ListenPort int
	// SelfNodeID excludes this node's own record from the peer set
	// (you don't peer with yourself).
	SelfNodeID string
}

// Controller abstracts the device so unit tests can mock netlink/wgctrl
// (real implementation: LinuxController).
type Controller interface {
	// EnsureDevice creates the interface if missing and sets its MTU.
	EnsureDevice(mtu int) error
	// Peers returns the currently configured peers.
	Peers() ([]PeerState, error)
	// SetPeer adds or updates exactly one peer, incrementally.
	SetPeer(spec PeerSpec) error
	// RemovePeer deletes exactly one peer by public key.
	RemovePeer(publicKey string) error
}

// Reconciler converges the exp0 peer set with the cluster store: the
// published peer records under /nodes/<id>/network.wgPublicKey plus VIP
// holder records under /network/vips/<addr>/holder.
type Reconciler struct {
	st   store.Store
	ctrl Controller
	cfg  Config
}

// NewReconciler creates a mesh reconciler.
func NewReconciler(st store.Store, ctrl Controller, cfg Config) *Reconciler {
	if cfg.Interface == "" {
		cfg.Interface = InterfaceName
	}
	if cfg.ListenPort == 0 {
		cfg.ListenPort = addrplan.WireGuardPort
	}
	return &Reconciler{st: st, ctrl: ctrl, cfg: cfg}
}

// Reconcile performs one convergence pass. Idempotent: a second call on
// a converged system produces no device operations.
func (r *Reconciler) Reconcile(ctx context.Context) error {
	peers, err := r.publishedPeers(ctx)
	if err != nil {
		return err
	}
	holders, err := r.vipHolders(ctx)
	if err != nil {
		return err
	}
	delete(peers, r.cfg.SelfNodeID)
	specs := BuildSpecs(peers, holders)

	if err := r.ctrl.EnsureDevice(r.mtu()); err != nil {
		return errors.New(errors.KindUnavailable, "mesh.ensureDevice", err.Error())
	}

	state, err := r.ctrl.Peers()
	if err != nil {
		return errors.New(errors.KindUnavailable, "mesh.peers", err.Error())
	}
	for _, op := range Diff(state, specs) {
		switch op.Kind {
		case OpRemove:
			if err := r.ctrl.RemovePeer(op.PublicKey); err != nil {
				return errors.New(errors.KindUnavailable, "mesh.removePeer", err.Error())
			}
		case OpAdd, OpUpdate:
			if err := r.ctrl.SetPeer(*op.Spec); err != nil {
				return errors.New(errors.KindUnavailable, "mesh.setPeer", err.Error())
			}
		}
	}
	return nil
}

// mtu computes exp0's MTU: override wins, else physical − 80, else the
// 1420 default.
func (r *Reconciler) mtu() int {
	return MTU(r.cfg.MTUOverride, r.physicalMTU())
}

func (r *Reconciler) physicalMTU() int {
	if r.cfg.PhysicalMTU > 0 {
		return r.cfg.PhysicalMTU
	}
	if m, err := detectPhysicalMTU(r.cfg.Interface); err == nil && m > 0 {
		return m
	}
	return 1500 // §4.1: default assumption when detection fails
}

// MTU is the pure MTU rule: override > 0 wins, else physical − 80 with
// a floor of the default (a physical MTU < 80 yields DefaultMTU).
func MTU(override, physical int) int {
	switch {
	case override > 0:
		return override
	case physical >= 80:
		return physical - 80
	default:
		return DefaultMTU
	}
}

// publishedPeers reads every /nodes/<id>/network.wgPublicKey record.
func (r *Reconciler) publishedPeers(ctx context.Context) (map[string]*proto.WireGuardPeer, error) {
	entries, err := r.st.List(ctx, "/nodes/")
	if err != nil {
		return nil, errors.New(errors.KindUnavailable, "mesh.listNodes", err.Error())
	}
	out := make(map[string]*proto.WireGuardPeer)
	for _, e := range entries {
		nodeID, ok := strings.CutSuffix(string(e.Key), "/network.wgPublicKey")
		if !ok || !strings.HasPrefix(nodeID, "/nodes/") {
			continue // not a mesh peer record
		}
		nodeID = strings.TrimPrefix(nodeID, "/nodes/")
		var p proto.WireGuardPeer
		if err := pbproto.Unmarshal(e.Value, &p); err != nil {
			return nil, errors.New(errors.KindInternal, "mesh.unmarshalPeer",
				fmt.Sprintf("%s: %v", e.Key, err))
		}
		out[nodeID] = &p
	}
	return out, nil
}

// vipHolders reads /network/vips/<addr>/holder records into
// holder nodeID → held VIP addresses. Invalid or holderless VIP keys are
// skipped (allocation records from T05 may exist without holders).
func (r *Reconciler) vipHolders(ctx context.Context) (map[string][]netip.Addr, error) {
	entries, err := r.st.List(ctx, "/network/vips/")
	if err != nil {
		return nil, errors.New(errors.KindUnavailable, "mesh.listVips", err.Error())
	}
	out := make(map[string][]netip.Addr)
	for _, e := range entries {
		rest, ok := strings.CutSuffix(string(e.Key), "/holder")
		if !ok {
			continue
		}
		rest = strings.TrimPrefix(rest, "/network/vips/")
		addr, addrErr := netip.ParseAddr(rest)
		if addrErr != nil {
			continue // non-address VIP key shape
		}
		holder := string(e.Value)
		out[holder] = append(out[holder], addr)
	}
	return out, nil
}

// BuildSpecs converts published peer records + VIP holders into desired
// peer specs. Self (this node's own record) is excluded — you don't peer
// with yourself. AllowedIPs = peer's overlay /24 (parsed from the
// record, which carries it so every node sees the same plan) plus each
// VIP it holds as /32.
func BuildSpecs(peers map[string]*proto.WireGuardPeer, holders map[string][]netip.Addr) []PeerSpec {
	specs := make([]PeerSpec, 0, len(peers))
	for nodeID, p := range peers {
		prefix, err := netip.ParsePrefix(p.OverlayPrefix)
		if err != nil {
			continue // malformed record; skip rather than wedge the mesh
		}
		ips := []netip.Prefix{prefix.Masked()}
		for _, vip := range holders[nodeID] {
			ips = append(ips, netip.PrefixFrom(vip, vip.BitLen()))
		}
		specs = append(specs, PeerSpec{
			NodeID:     nodeID,
			PublicKey:  p.PublicKey,
			Endpoint:   p.Endpoint,
			AllowedIPs: ips,
			Keepalive:  KeepaliveInterval,
		})
	}
	return specs
}
