// Live collectors for `expanse doctor network` (§5). Each function
// maps OS state into the Input fields; all are best-effort — a missing
// tool or permission degrades that row to SKIP/WARN, never panics.
package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/expanse/expanse/internal/network/firewall"
	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/vishvananda/netlink"
)

// Peer is one cluster node to probe.
type Peer struct {
	Node         string
	Overlay      netip.Addr // 10.42.N.1
	ClusterPorts []int      // e.g. 7443–7446
}

// CollectLive fills Input for the given peers/VIPs/blocks. It never
// returns an error: rows that cannot run are surfaced via the checks'
// SKIP/WARN paths instead.
func CollectLive(ctx context.Context, peers []Peer, vips []netip.Addr, blocks []string) Input {
	in := Input{PortMatrix: map[string]map[int]error{}}

	// 1+2: links.
	links, err := netlink.LinkList()
	if err == nil {
		for _, l := range links {
			li := LinkInfo{Name: l.Attrs().Name, Up: l.Attrs().OperState == netlink.OperUp || l.Attrs().Flags&net.FlagUp != 0, MTU: l.Attrs().MTU}
			addrs, err := netlink.AddrList(l, netlink.FAMILY_V4)
			if err == nil {
				for _, a := range addrs {
					if pfx, err := netip.ParsePrefix(a.IPNet.String()); err == nil {
						li.Addrs = append(li.Addrs, pfx)
					}
				}
			}
			in.Links = append(in.Links, li)
			if li.Name == "exp0" {
				in.Exp0 = li
				in.Exp0MTU = 1420 // §3 wireguard MTU
			}
		}
	}

	// 3+4: overlay pings + DF ping against the first peer.
	for _, p := range peers {
		pp := PeerPing{Node: p.Node, Overlay: p.Overlay}
		start := time.Now()
		out, err := runCheck("ping", "-c", "1", "-W", "2", p.Overlay.String())
		pp.Latency = time.Since(start)
		if err != nil {
			pp.Err = firstLine(out)
		}
		in.PeerPings = append(in.PeerPings, pp)
	}
	if len(peers) > 0 {
		in.DFPingErr = collectDFPing(peers[0].Overlay)
	}

	// 5: TCP connect matrix to cluster ports.
	for _, p := range peers {
		m := map[int]error{}
		for _, port := range p.ClusterPorts {
			d := net.Dialer{Timeout: 2 * time.Second}
			conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(p.Overlay.String(), strconv.Itoa(port)))
			if err == nil {
				conn.Close()
			}
			m[port] = err
		}
		in.PortMatrix[p.Node] = m
	}

	// 6+7: VIP holders (who has the address configured) + ARP MACs.
	for _, vip := range vips {
		st := VIPState{Addr: vip, ARPMACs: map[string]string{}}
		if hasAddr(vip) {
			st.Holders = append(st.Holders, "self")
		}
		// ARP entry for the VIP from the local neighbor table, if any.
		if mac := vipMAC(vip); mac != "" {
			st.ARPMACs["self"] = mac
		}
		in.VIPs = append(in.VIPs, st)
	}

	// 8: resolve every block name locally.
	r := &net.Resolver{PreferGo: true}
	for _, name := range blocks {
		ctx2, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := r.LookupHost(ctx2, name)
		cancel()
		in.DNSProbes = append(in.DNSProbes, DNSProbe{Node: "self", Name: name, Err: errString(err)})
	}

	// 9: upstream DNS.
	in.UpstreamErr = collectUpstreamDNS()

	// 10: firewall ruleset + drop counters.
	if err := countFirewall(&in.FwLoaded, &in.FwDrops); err != nil {
		in.FwErr = err.Error()
	}

	// 11: conntrack.
	in.ConntrackUsed, in.ConntrackMax = collectConntrack()

	// 12: local clock offset via chrony (per-node run covers the fleet).
	if out, err := runCheck("chronyc", "tracking"); err == nil {
		if off := parseChronyOffset(out); off > 100*time.Millisecond {
			in.TimeOffsets = append(in.TimeOffsets, NodeOffset{Node: "self", Offset: off})
		}
	}
	return in
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// hasAddr reports whether the local system holds the address.
func hasAddr(addr netip.Addr) bool {
	links, err := netlink.LinkList()
	if err != nil {
		return false
	}
	for _, l := range links {
		addrs, err := netlink.AddrList(l, netlink.FAMILY_V4)
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ip, ok := netip.AddrFromSlice(a.IP); ok && ip == addr {
				return true
			}
		}
	}
	return false
}

// vipMAC returns the MAC the neighbor table has for addr (LLADDR of
// the VIP's ARP entry), "" if none.
func vipMAC(addr netip.Addr) string {
	out, err := runCheck("ip", "-j", "neigh", "show", "to", addr.String())
	if err != nil {
		return ""
	}
	var entries []map[string]any
	if json.Unmarshal([]byte(out), &entries) != nil {
		return ""
	}
	for _, e := range entries {
		if lladdr, ok := e["lladdr"].(string); ok {
			return lladdr
		}
	}
	return ""
}

// countFirewall checks the §4.5 table exists and sums drop counters
// from the main input chain (visible via `nft list chain` output).
func countFirewall(loaded *bool, drops *map[string]uint64) error {
	c, err := nftables.New()
	if err != nil {
		return fmt.Errorf("nftables: %w", err)
	}
	table := &nftables.Table{Family: nftables.TableFamilyINet, Name: firewall.Table}
	chains, err := c.ListChains()
	if err != nil {
		return fmt.Errorf("nftables: %w", err)
	}
	d := map[string]uint64{}
	for _, ch := range chains {
		if ch.Name != firewall.Chain || ch.Table.Name != firewall.Table {
			continue
		}
		*loaded = true
		rules, err := c.GetRules(table, ch)
		if err != nil {
			continue
		}
		for _, r := range rules {
			for _, e := range r.Exprs {
				if ctr, ok := e.(*expr.Counter); ok && ctr.Packets > 0 {
					d[firewall.Chain] += ctr.Packets
				}
			}
		}
	}
	// Policy chain drops are the interesting ones; count via chain name.
	for _, ch := range chains {
		if ch.Name == firewall.PolicyChain && ch.Table.Name == firewall.Table {
			*loaded = true
		}
	}
	*drops = d
	return nil
}

// parseChronyOffset extracts "System time: X seconds ... of NTP time".
func parseChronyOffset(out string) time.Duration {
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "System time") {
			continue
		}
		f := strings.Fields(line)
		for i, w := range f {
			if sec, err := strconv.ParseFloat(w, 64); err == nil && i > 0 {
				return time.Duration(sec * float64(time.Second))
			}
		}
	}
	return 0
}
