// mdns.go is the mDNS bridge (T17, §4.4): blocks that opt in via
// network.mdns are additionally answered as <block>.<ns>.local via
// multicast on the LAN, so clients that never configured DNS can still
// reach them. The bridge re-announces on every zone change (the set of
// announced names/IPs follows the authoritative zone) and answers A
// queries only — service discovery via SRV stays on unicast DNS.
package dns

import (
	"context"
	"net"
	"net/netip"
	"sort"
	"strings"
	"time"

	mdns "github.com/hashicorp/mdns"
)

// mdnsScanInterval is how often the bridge re-checks the zone for
// opt-in changes. The underlying server joins the multicast group;
// restarting it wholesale on a plan change is simple and correct.
const mdnsScanInterval = 5 * time.Second

// MDNSPlan is one block's LAN announcement: the name (with trailing
// dot) answered on .local, and the addresses to answer with (the same
// IPs the authoritative zone serves — VIP if allocated, else ready
// replica node IPs).
type MDNSPlan struct {
	Name string   // "<block>.<ns>.local."
	IPs  []net.IP // deduplicated, sorted
}

// MDNSPlans derives the bridge's announcement set from the current
// zone input. It is a pure function so the mapping (opt-in blocks →
// .local names → IPs) is testable without joining a multicast group.
func MDNSPlans(in Input) []MDNSPlan {
	var out []MDNSPlan
	for _, b := range in.Blocks {
		if !b.MDNS || b.Namespace == "" || b.Name == "" || len(b.Replicas) == 0 {
			continue
		}
		ready := 0
		seen := map[netip.Addr]bool{}
		var ips []net.IP
		if b.VIP.IsValid() {
			a := b.VIP.Addr()
			v4 := a.As4()
			ips = append(ips, net.IP(v4[:]))
			ready = 1
		} else {
			for _, r := range b.Replicas {
				if !r.Ready || r.Node == nil || len(r.Node.Addrs) == 0 {
					continue
				}
				ready++
				a := r.Node.Addrs[0]
				if seen[a] {
					continue
				}
				seen[a] = true
				v4 := a.As4()
				ips = append(ips, net.IP(v4[:]))
			}
		}
		if ready == 0 {
			continue // nothing ready → announce nothing
		}
		sort.Slice(ips, func(i, j int) bool { return ipLess(ips[i], ips[j]) })
		out = append(out, MDNSPlan{
			Name: strings.ToLower(b.Name + "." + b.Namespace + ".local."),
			IPs:  ips,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func ipLess(a, b net.IP) bool {
	a4, b4 := a.To4(), b.To4()
	for i := range 4 {
		if a4[i] != b4[i] {
			return a4[i] < b4[i]
		}
	}
	return false
}

// RunMDNS keeps the LAN announcements in step with the zone: every
// scan interval it re-derives the plans and, if they changed, restarts
// the multicast server. announce is a seam for tests (pass nil to use
// the real hashicorp/mdns server).
func RunMDNS(ctx context.Context, plans func() []MDNSPlan, announce func(MDNSPlan) (stop func(), err error)) error {
	if announce == nil {
		announce = startMDNS
	}
	var stops []func()
	cur := []MDNSPlan(nil)
	defer func() {
		for _, s := range stops {
			s()
		}
	}()
	for {
		want := plans()
		if !plansEqual(want, cur) {
			for _, s := range stops {
				s()
			}
			stops = nil
			ok := true
			for _, p := range want {
				stop, err := announce(p)
				if err != nil {
					ok = false // one bad announcement must not kill the bridge
					continue
				}
				stops = append(stops, stop)
			}
			if ok {
				cur = want
			} else {
				cur = nil // retry the whole set next tick
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(mdnsScanInterval):
		}
	}
}

// startMDNS announces one plan via multicast. The service instance is
// the announced name itself; hashicorp/mdns answers A queries for the
// host, which is all the bridge promises.
func startMDNS(p MDNSPlan) (func(), error) {
	svc, err := mdns.NewMDNSService(p.Name, "_expanse._tcp", "local.", p.Name, 0, p.IPs, nil)
	if err != nil {
		return nil, err
	}
	srv, err := mdns.NewServer(&mdns.Config{Zone: svc})
	if err != nil {
		return nil, err
	}
	return func() { _ = srv.Shutdown() }, nil
}

func plansEqual(a, b []MDNSPlan) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name {
			return false
		}
		if len(a[i].IPs) != len(b[i].IPs) {
			return false
		}
		for j := range a[i].IPs {
			if !a[i].IPs[j].Equal(b[i].IPs[j]) {
				return false
			}
		}
	}
	return true
}
