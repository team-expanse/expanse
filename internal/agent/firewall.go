// firewall.go drives the §4.5 nftables ruleset from the store: one
// watch on "/", then rebuild the four dynamic sets' desired contents
// and push element-only diffs (D5.6 — never a full reload). Cluster
// nodes only; failures log and retry, they never take the node down
// (a firewall hiccup must not wedge the cluster).
package agent

import (
	"context"
	"encoding/json"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/google/nftables"

	fw "github.com/expanse/expanse/internal/network/firewall"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
)

// fwPeriod is the resync backstop: watch-driven with a periodic
// re-diff so a missed event or failed op self-heals.
const fwPeriod = 5 * time.Second

func (a *Agent) initFirewall(ctx context.Context) {
	go a.firewallLoop(ctx)
}

func (a *Agent) firewallLoop(ctx context.Context) {
	conn := &nftables.Conn{}
	if err := fw.Bootstrap(conn); err != nil {
		a.logger.Warn("firewall bootstrap failed; firewall disabled", "err", err)
		return
	}
	a.logger.Info("firewall ruleset bootstrapped", "table", "inet expanse")

	for {
		if err := a.fwSync(ctx, conn); err != nil {
			a.logger.Warn("firewall set sync failed; retrying", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(fwPeriod):
		}
	}
}

// fwSync recomputes the desired dynamic sets from the store and diffs
// them into the kernel.
func (a *Agent) fwSync(ctx context.Context, conn *nftables.Conn) error {
	d, err := a.fwDesired(ctx)
	if err != nil {
		return err
	}
	return fw.Sync(conn, d)
}

// fwDesired reads node peer records, VIP allocations, and block
// declarations from the store (fresh List each pass — the store is
// small and this avoids watch bookkeeping).
func (a *Agent) fwDesired(ctx context.Context) (fw.Desired, error) {
	d := fw.Desired{}

	// Cluster peers: overlay host address (10.42.<idx>.1) plus the
	// node's LAN endpoint, so raft/control-plane traffic is admitted
	// over both paths.
	peers, err := a.store.List(ctx, store.Key("/nodes/"))
	if err != nil {
		return d, err
	}
	for _, e := range peers {
		rest := strings.TrimPrefix(string(e.Key), "/nodes/")
		// Node records (JSON, created at join time) carry each peer's
		// raft/API addresses — they exist before the wg record does,
		// which breaks the join deadlock: the new node dials 7444 over
		// the LAN only after its record reaches this node's set.
		if !strings.Contains(rest, "/") {
			var rec struct {
				RaftAddr string `json:"raft_addr"`
				APIAddr  string `json:"api_addr"`
			}
			if json.Unmarshal(e.Value, &rec) == nil {
				for _, cand := range []string{rec.RaftAddr, rec.APIAddr} {
					if host, _, err := net.SplitHostPort(cand); err == nil && host != "0.0.0.0" {
						if ip, err := netip.ParseAddr(host); err == nil && ip.Is4() {
							d.Peers = append(d.Peers, ip)
						}
					}
				}
			}
			continue
		}
		if !strings.HasSuffix(string(e.Key), "/network.wgPublicKey") {
			continue
		}
		var peer pb.WireGuardPeer
		if err := proto.Unmarshal(e.Value, &peer); err != nil {
			continue // malformed record: skip, not fatal
		}
		if peer.OverlayPrefix != "" {
			if pfx, err := netip.ParsePrefix(peer.OverlayPrefix); err == nil && pfx.Addr().Is4() {
				b := pfx.Addr().As4()
				b[3] = 1 // §3: the node's own address is base+1
				d.Peers = append(d.Peers, netip.AddrFrom4(b))
			}
		}
		if host, err := netip.ParseAddrPort(peer.Endpoint); err == nil && host.Addr().Is4() {
			d.Peers = append(d.Peers, host.Addr())
		}
	}

	// VIP allocations (any scope — any node may hold any VIP after
	// failover).
	vips, err := a.store.List(ctx, store.Key("/network/vipPool/"))
	if err != nil {
		return d, err
	}
	for _, e := range vips {
		var rec struct {
			Addr string `json:"addr"`
		}
		if err := json.Unmarshal(e.Value, &rec); err != nil || rec.Addr == "" {
			continue
		}
		// The allocation's addr is a CIDR prefix string whose host bits
		// carry the VIP ("192.168.1.100/24") — do NOT mask.
		if pfx, err := netip.ParsePrefix(rec.Addr); err == nil && pfx.Addr().Is4() {
			d.VIPs = append(d.VIPs, pfx)
		}
	}

	// Per-block declared ports on VIP addresses (§4.5: only EXPOSE_VIP
	// ports are admitted; the dport is the service's declared port).
	blocks, err := a.store.List(ctx, store.Key("/blocks/"))
	if err != nil {
		return d, err
	}
	for _, e := range blocks {
		parts := strings.Split(string(e.Key), "/")
		// /blocks/<ns>/<name> (spec); skip deeper status keys.
		if len(parts) != 4 {
			continue
		}
		var b pb.Block
		if err := proto.Unmarshal(e.Value, &b); err != nil {
			continue
		}
		for _, p := range b.GetSpec().GetNetwork().GetPorts() {
			if p.GetExpose() != pb.Expose_EXPOSE_VIP {
				continue
			}
			switch strings.ToLower(p.GetProtocol()) {
			case "tcp":
				d.TCPPorts = append(d.TCPPorts, uint16(p.GetPort()))
			case "udp":
				d.UDPPorts = append(d.UDPPorts, uint16(p.GetPort()))
			}
		}
	}
	return d, nil
}
