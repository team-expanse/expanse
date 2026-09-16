// Package doctor implements `expanse doctor network` (§5): 12 checks
// covering every networking subsystem, reported as a PASS/WARN/FAIL
// table with one remediation hint per row.
//
// Structure: Collect() gathers live system state into an Input struct
// (netlink, wgctrl, ICMP via ping(8), TCP dials, DNS, nftables,
// conntrack, chrony). Each checkNN function is a pure decision over
// Input — that is what the unit tests cover; the collectors are thin
// OS adapters exercised by the VM tests.
package doctor

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Status of one check.
type Status string

const (
	Pass Status = "PASS"
	Warn Status = "WARN"
	Fail Status = "FAIL"
	Skip Status = "SKIP" // collector could not run (missing tool, no perms)
)

// Result is one row of the doctor table.
type Result struct {
	Check  string
	Status Status
	Detail string // what was observed
	Hint   string // remediation, shown for WARN/FAIL/SKIP
}

// Input is everything the 12 checks decide over. Collect fills it;
// tests construct it by hand.
type Input struct {
	// 1: interfaces up with addresses.
	Links []LinkInfo
	// 2: exp0 state (zero MTU/addr = missing).
	Exp0    LinkInfo
	Exp0MTU int
	// 3: overlay ping per peer node.
	PeerPings []PeerPing
	// 4: 1400-byte DF ping across the mesh.
	DFPingErr string
	// 5: cluster TCP connect matrix, per node per port.
	PortMatrix map[string]map[int]error
	// 6/7: VIP holder node ids per VIP, and ARP MACs observed for it.
	VIPs []VIPState
	// 8: block DNS resolution per node.
	DNSProbes []DNSProbe
	// 9: upstream DNS reachability.
	UpstreamErr string
	// 10: firewall ruleset presence and drop counters per set/chain.
	FwLoaded bool
	FwDrops  map[string]uint64
	FwErr    string
	// 11: conntrack utilization.
	ConntrackUsed, ConntrackMax int
	// 12: per-node clock offsets.
	TimeOffsets []NodeOffset
}

type LinkInfo struct {
	Name  string
	Up    bool
	MTU   int
	Addrs []netip.Prefix
}

type PeerPing struct {
	Node    string
	Overlay netip.Addr
	Latency time.Duration // >0 on success
	Err     string
}

type VIPState struct {
	Addr    netip.Addr
	Holders []string          // node ids that report holding it
	ARPMACs map[string]string // node id → MAC seen for the VIP
}

type DNSProbe struct {
	Node string
	Name string
	Err  string
}

type NodeOffset struct {
	Node   string
	Offset time.Duration // absolute value
	Err    string
}

// Run executes all 12 checks in §5 order.
func Run(in Input) []Result {
	return []Result{
		checkInterfaces(in.Links),
		checkExp0(in.Exp0, in.Exp0MTU),
		checkOverlayPeers(in.PeerPings),
		checkDFPing(in.DFPingErr),
		checkPortMatrix(in.PortMatrix),
		checkVIPHolders(in.VIPs),
		checkARPSanity(in.VIPs),
		checkBlockDNS(in.DNSProbes),
		checkUpstreamDNS(in.UpstreamErr),
		checkFirewall(in.FwLoaded, in.FwDrops, in.FwErr),
		checkConntrack(in.ConntrackUsed, in.ConntrackMax),
		checkTimeSync(in.TimeOffsets),
	}
}

func checkInterfaces(links []LinkInfo) Result {
	var down []string
	for _, l := range links {
		if !l.Up {
			down = append(down, l.Name)
		}
	}
	if len(down) > 0 {
		return Result{
			"interfaces", Fail,
			"down: " + strings.Join(down, ","),
			"check cable/switch and run `ip link set <if> up`",
		}
	}
	return Result{"interfaces", Pass, fmt.Sprintf("%d links up", len(links)), ""}
}

func checkExp0(exp0 LinkInfo, wantMTU int) Result {
	if exp0.Name == "" || !exp0.Up {
		return Result{
			"exp0", Fail, "exp0 missing or down",
			"restart the expanse agent (mesh reconciler creates exp0); check WireGuard key in /nodes",
		}
	}
	if exp0.MTU != wantMTU {
		return Result{
			"exp0", Fail,
			fmt.Sprintf("MTU %d, want %d", exp0.MTU, wantMTU),
			fmt.Sprintf("ip link set exp0 mtu %d — wrong MTU breaks 1400-byte DF traffic", wantMTU),
		}
	}
	if len(exp0.Addrs) == 0 {
		return Result{
			"exp0", Fail, "no address on exp0",
			"agent must hold 10.42.N.1/24 — check /network/nodeIndexes claim",
		}
	}
	return Result{
		"exp0", Pass,
		fmt.Sprintf("up, MTU %d, %s", exp0.MTU, exp0.Addrs[0]), "",
	}
}

func checkOverlayPeers(pings []PeerPing) Result {
	if len(pings) == 0 {
		return Result{"overlay-peers", Warn, "no peers to probe (single node?)", ""}
	}
	var bad []string
	var lat []string
	for _, p := range pings {
		if p.Err != "" {
			bad = append(bad, p.Node)
			continue
		}
		lat = append(lat, fmt.Sprintf("%s %.1fms", p.Node, float64(p.Latency.Microseconds())/1000))
	}
	if len(bad) > 0 {
		return Result{
			"overlay-peers", Fail, "unreachable: " + strings.Join(bad, ","),
			"ping 10.42.N.1 manually; check wg show exp0 peers and nftables rules",
		}
	}
	// >5 ms over a LAN mesh is unusual but not broken.
	for _, p := range pings {
		if p.Latency > 5*time.Millisecond {
			return Result{
				"overlay-peers", Warn, "latency >5ms: " + strings.Join(lat, " "),
				"investigate switch/route flapping before trusting failover timings",
			}
		}
	}
	return Result{"overlay-peers", Pass, strings.Join(lat, " "), ""}
}

func checkDFPing(errStr string) Result {
	if errStr != "" {
		return Result{
			"df-mtu", Fail, errStr,
			"a 1400-byte DF ping must fit in the overlay MTU — check `ip link show exp0` on both ends",
		}
	}
	return Result{"df-mtu", Pass, "1400-byte DF ping ok", ""}
}

func checkPortMatrix(m map[string]map[int]error) Result {
	if len(m) == 0 {
		return Result{"port-matrix", Warn, "no peers to probe", ""}
	}
	var bad []string
	for node, ports := range m {
		var portNums []int
		for p := range ports {
			portNums = append(portNums, p)
		}
		sort.Ints(portNums)
		for _, p := range portNums {
			if ports[p] != nil {
				bad = append(bad, fmt.Sprintf("%s:%d", node, p))
			}
		}
	}
	if len(bad) > 0 {
		return Result{
			"port-matrix", Fail, "unreachable: " + strings.Join(bad, ","),
			"check §4.5 firewall sets (cluster_peers) and that every node runs the same ports",
		}
	}
	return Result{
		"port-matrix", Pass,
		fmt.Sprintf("all cluster ports reachable on %d nodes", len(m)), "",
	}
}

func checkVIPHolders(vips []VIPState) Result {
	if len(vips) == 0 {
		return Result{"vips", Pass, "no VIPs allocated", ""}
	}
	var bad []string
	for _, v := range vips {
		switch n := len(v.Holders); {
		case n == 0:
			bad = append(bad, v.Addr.String()+" unheld")
		case n > 1:
			bad = append(bad, fmt.Sprintf("%s held by %s", v.Addr, strings.Join(v.Holders, ",")))
		}
	}
	if len(bad) > 0 {
		return Result{
			"vips", Fail, strings.Join(bad, "; "),
			"unheld: check lease holder liveness; duplicate: split-brain — check raft health first",
		}
	}
	var parts []string
	for _, v := range vips {
		parts = append(parts, v.Addr.String()+"@"+v.Holders[0])
	}
	return Result{"vips", Pass, strings.Join(parts, " "), ""}
}

func checkARPSanity(vips []VIPState) Result {
	// Every node observes the holder's MAC for the VIP (identical —
	// that's healthy). The anomaly is the same VIP resolving to
	// *distinct* MACs from different vantage points: two nodes both
	// owning the address (split brain).
	for _, v := range vips {
		macs := map[string]bool{}
		for _, mac := range v.ARPMACs {
			if mac != "" {
				macs[strings.ToLower(mac)] = true
			}
		}
		if len(macs) > 1 {
			seen := make([]string, 0, len(macs))
			for mac := range macs {
				seen = append(seen, mac)
			}
			sort.Strings(seen)
			return Result{
				"arp", Fail,
				fmt.Sprintf("%s resolves to %d MACs (%s)", v.Addr, len(seen), strings.Join(seen, ",")),
				"two nodes answering for one VIP — stop the expanse agents, clear ARP (ip neigh flush), restart",
			}
		}
	}
	return Result{"arp", Pass, "no duplicate MACs for VIPs", ""}
}

func checkBlockDNS(probes []DNSProbe) Result {
	if len(probes) == 0 {
		return Result{"block-dns", Warn, "no blocks to resolve", ""}
	}
	var bad []string
	for _, p := range probes {
		if p.Err != "" {
			bad = append(bad, p.Node+"/"+p.Name)
		}
	}
	if len(bad) > 0 {
		return Result{
			"block-dns", Fail, "failed: " + strings.Join(bad, ","),
			"dnsmasq runs on every node (127.0.0.53) — check dnsmasq.service and zone source updates",
		}
	}
	return Result{
		"block-dns", Pass,
		fmt.Sprintf("%d block names resolve on all nodes", len(probes)), "",
	}
}

func checkUpstreamDNS(errStr string) Result {
	if errStr != "" {
		return Result{
			"upstream-dns", Warn, errStr,
			"block image pulls need upstream DNS — check /etc/resolv.conf forwarding in dnsmasq",
		}
	}
	return Result{"upstream-dns", Pass, "reachable", ""}
}

func checkFirewall(loaded bool, drops map[string]uint64, errStr string) Result {
	if errStr != "" {
		return Result{
			"firewall", Warn, errStr,
			"agent --firewall not running? start it or silence this check",
		}
	}
	if !loaded {
		return Result{
			"firewall", Fail, "expanse nftables table missing",
			"start the agent with --firewall; check `nft list table inet expanse`",
		}
	}
	var parts []string
	var names []string
	for n := range drops {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%s=%d drops", n, drops[n]))
	}
	return Result{"firewall", Pass, "ruleset loaded; " + strings.Join(parts, ", "), ""}
}

func checkConntrack(used, max int) Result {
	if max <= 0 {
		return Result{
			"conntrack", Warn, "conntrack accounting unavailable",
			"check sysctl net.netfilter.nf_conntrack_max",
		}
	}
	pct := 100 * used / max
	detail := fmt.Sprintf("%d/%d entries (%d%%)", used, max, pct)
	switch {
	case pct >= 90:
		return Result{
			"conntrack", Fail, detail,
			"table nearly full — raise net.netfilter.nf_conntrack_max or reduce LB connection churn",
		}
	case pct >= 70:
		return Result{
			"conntrack", Warn, detail,
			"watch growth; raise net.netfilter.nf_conntrack_max before it saturates",
		}
	default:
		return Result{"conntrack", Pass, detail, ""}
	}
}

func checkTimeSync(offsets []NodeOffset) Result {
	const limit = 100 * time.Millisecond
	var bad []string
	for _, o := range offsets {
		if o.Err != "" {
			bad = append(bad, o.Node+" ("+o.Err+")")
			continue
		}
		if o.Offset > limit {
			bad = append(bad, fmt.Sprintf("%s %s", o.Node, o.Offset))
		}
	}
	if len(bad) > 0 {
		return Result{
			"time-sync", Fail, "offset >100ms: " + strings.Join(bad, ","),
			"lease tokens and certificate lifetimes skew — enable NTP on every node",
		}
	}
	return Result{"time-sync", Pass, fmt.Sprintf("all nodes within %s", limit), ""}
}

// Format renders the results as the doctor table.
func Format(rs []Result) string {
	w := len("CHECK")
	for _, r := range rs {
		if len(r.Check) > w {
			w = len(r.Check)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-*s  %-4s  %s\n", w, "CHECK", "STAT", "DETAIL")
	for _, r := range rs {
		fmt.Fprintf(&b, "%-*s  %-4s  %s\n", w, r.Check, r.Status, r.Detail)
		if r.Hint != "" {
			fmt.Fprintf(&b, "%-*s  %s\n", w, "", "↳ "+r.Hint)
		}
	}
	return b.String()
}

// Collectors -------------------------------------------------------------

// runCheck wraps a collector command, returning its combined output.
func runCheck(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

// collectDFPing runs a 1400-byte DF ping (ping(8) supports -M do) over
// the overlay address of a peer.
func collectDFPing(peer netip.Addr) string {
	if peer.IsValid() && peer.Is4() {
		out, err := runCheck("ping", "-c", "1", "-W", "2", "-M", "do",
			"-s", "1372", peer.String()) // 1372 payload + 28 hdr = 1400 on wire
		if err != nil {
			return fmt.Sprintf("DF ping %s failed: %s", peer, firstLine(out))
		}
	}
	return ""
}

// collectConntrack reads utilization from /proc and sysctl.
func collectConntrack() (used, max int) {
	if data, err := exec.Command("sysctl", "-n",
		"net.netfilter.nf_conntrack_max").Output(); err == nil {
		max, _ = strconv.Atoi(strings.TrimSpace(string(data)))
	}
	if data, err := exec.Command("conntrack", "-S").Output(); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			f := strings.Fields(line)
			// "cpu=0 ... found=123 invalid=0 insert=0 ..."
			for _, kv := range f {
				if strings.HasPrefix(kv, "found=") {
					if n, err := strconv.Atoi(strings.TrimPrefix(kv, "found=")); err == nil {
						used += n
					}
				}
			}
		}
	}
	return
}

// collectUpstreamDNS probes the system resolver with a DNS lookup of
// a well-known name through a plain-IP dial (no search-path games).
func collectUpstreamDNS() string {
	servers := resolvConfServers()
	if len(servers) == 0 {
		return "no nameservers in /etc/resolv.conf"
	}
	for _, srv := range servers {
		r := &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
				d := net.Dialer{Timeout: 3 * time.Second}
				return d.DialContext(ctx, "udp", net.JoinHostPort(srv, "53"))
			},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		addrs, err := r.LookupNetIP(ctx, "ip4", "example.com.")
		cancel()
		if err == nil && len(addrs) > 0 {
			return ""
		}
	}
	return "upstream DNS unreachable: " + strings.Join(servers, ",")
}

// resolvConfServers parses "nameserver" lines from /etc/resolv.conf.
func resolvConfServers() []string {
	data, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "nameserver" {
			out = append(out, f[1])
		}
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
