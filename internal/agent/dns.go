// dns.go wires cluster DNS into the agent (T17, PHASE05.md §4.4): the
// zone source rebuilds the record set from the store, the server
// answers authoritatively on 127.0.0.53:53 and the node's overlay
// address, and everything else is forwarded to the node's configured
// resolvers. The mDNS bridge announces opted-in blocks on the LAN.
//
// Startup failures are logged and retried, never fatal: DNS is a
// convenience layer over the store, and a node with a broken listener
// still participates in the cluster. /etc/resolv.conf pointing at
// 127.0.0.53 is arranged by the packaging (T18), not by the agent —
// overwriting a live resolv.conf from a service is the wrong layer.
package agent

import (
	"bufio"
	"context"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/expanse/expanse/internal/network/addrplan"
	"github.com/expanse/expanse/internal/network/dns"
	"github.com/expanse/expanse/internal/network/mesh"
	miekgdns "github.com/miekg/dns"
)

// dnsUpstreamFallback is used when /etc/resolv.conf yields no usable
// upstream (e.g. it only contains the stub address).
const dnsUpstreamFallback = "1.1.1.1:53"

// dnsUpstreamTimeouts names the resolv.conf lines we skip.
var dnsSkipNameservers = map[string]bool{
	"127.0.0.53": true, // this server's own address
	"127.0.0.1":  true,
}

// initDNS starts the zone source, the listeners, and the mDNS bridge.
// Called from Run; every piece runs in its own retried goroutine.
func (a *Agent) initDNS(ctx context.Context) {
	srv := dns.NewServer()
	srv.Upstreams = dnsUpstreams()

	zs := dns.NewZoneSource(a.store, srv)
	go func() {
		for {
			if err := zs.Run(ctx); err != nil && ctx.Err() == nil {
				a.logger.Warn("dns zone source stopped; restarting", "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}()

	// mDNS bridge: derive plans from the zone source's last input.
	go func() {
		err := dns.RunMDNS(ctx, func() []dns.MDNSPlan {
			return dns.MDNSPlans(zs.LastInput())
		}, nil)
		if err != nil && ctx.Err() == nil {
			a.logger.Warn("mdns bridge stopped", "err", err)
		}
	}()

	// Listeners: loopback + the node's overlay address (10.42.N.1 —
	// the address the mesh puts on exp0). UDP and TCP each.
	addrs := []string{"127.0.0.53:53"}
	if idx, err := mesh.ClaimIndex(ctx, a.store, a.cfg.NodeID); err == nil {
		if pfx, err := addrplan.OverlayPrefix(idx); err == nil {
			addrs = append(addrs, netip.PrefixFrom(pfx.Addr(), 32).Addr().String()+":53")
		}
	}
	for _, addr := range addrs {
		for _, netw := range []string{"udp", "tcp"} {
			go func(addr, netw string) {
				s := &miekgdns.Server{Addr: addr, Net: netw, Handler: srv}
				if err := s.ListenAndServe(); err != nil && ctx.Err() == nil {
					a.logger.Warn("dns listener failed", "net", netw, "addr", addr, "err", err)
				}
			}(addr, netw)
		}
	}
}

// dnsUpstreams reads the node's resolvers from /etc/resolv.conf,
// skipping our own stub address.
func dnsUpstreams() []string {
	var out []string
	f, err := os.Open("/etc/resolv.conf")
	if err != nil {
		return []string{dnsUpstreamFallback}
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "nameserver ") {
			continue
		}
		ns := strings.TrimSpace(strings.TrimPrefix(line, "nameserver "))
		if ns == "" || dnsSkipNameservers[ns] {
			continue
		}
		if a, err := netip.ParseAddr(ns); err == nil && a.Is4() {
			out = append(out, ns+":53")
		}
	}
	if len(out) == 0 {
		return []string{dnsUpstreamFallback}
	}
	return out
}
