package doctor

import (
	"net/netip"
	"strings"
	"testing"
	"time"
)

func status(t *testing.T, r Result, want Status) Result {
	t.Helper()
	if r.Status != want {
		t.Fatalf("status = %s (detail %q), want %s", r.Status, r.Detail, want)
	}
	return r
}

func TestCheckInterfaces(t *testing.T) {
	up := []LinkInfo{{Name: "eth1", Up: true}, {Name: "exp0", Up: true}}
	status(t, checkInterfaces(up), Pass)
	status(t, checkInterfaces([]LinkInfo{{Name: "eth1"}}), Fail)
}

func TestCheckExp0(t *testing.T) {
	ok := LinkInfo{
		Name: "exp0", Up: true, MTU: 1420,
		Addrs: []netip.Prefix{netip.MustParsePrefix("10.42.1.1/24")},
	}
	status(t, checkExp0(ok, 1420), Pass)
	status(t, checkExp0(LinkInfo{}, 1420), Fail)
	status(t, checkExp0(LinkInfo{
		Name: "exp0", Up: true, MTU: 1500,
		Addrs: ok.Addrs,
	}, 1420), Fail)
	status(t, checkExp0(LinkInfo{Name: "exp0", Up: true, MTU: 1420}, 1420), Fail)
}

func TestCheckOverlayPeers(t *testing.T) {
	good := []PeerPing{
		{Node: "n2", Overlay: netip.MustParseAddr("10.42.2.1"), Latency: 300 * time.Microsecond},
		{Node: "n3", Overlay: netip.MustParseAddr("10.42.3.1"), Latency: time.Millisecond},
	}
	status(t, checkOverlayPeers(good), Pass)
	status(t, checkOverlayPeers(nil), Warn)
	bad := append(good, PeerPing{Node: "n9", Err: "timeout"})
	status(t, checkOverlayPeers(bad), Fail)
	slow := append(good[:0:0], PeerPing{Node: "n2", Overlay: good[0].Overlay, Latency: 20 * time.Millisecond})
	status(t, checkOverlayPeers(slow), Warn)
}

func TestCheckDFPing(t *testing.T) {
	status(t, checkDFPing(""), Pass)
	status(t, checkDFPing("DF ping 10.42.2.1 failed: exit 1"), Fail)
}

func TestCheckPortMatrix(t *testing.T) {
	m := map[string]map[int]error{
		"n2": {7443: nil, 7444: nil},
		"n3": {7443: nil, 7444: nil},
	}
	status(t, checkPortMatrix(m), Pass)
	m["n3"][7444] = errConn{}
	status(t, checkPortMatrix(m), Fail)
	status(t, checkPortMatrix(map[string]map[int]error{}), Warn)
}

type errConn struct{}

func (errConn) Error() string { return "refused" }

func TestCheckVIPHolders(t *testing.T) {
	vip := netip.MustParseAddr("192.168.1.100")
	one := []VIPState{{Addr: vip, Holders: []string{"n1"}}}
	status(t, checkVIPHolders(one), Pass)
	status(t, checkVIPHolders(nil), Pass)
	status(t, checkVIPHolders([]VIPState{{Addr: vip}}), Fail)
	status(t, checkVIPHolders([]VIPState{{Addr: vip, Holders: []string{"n1", "n2"}}}), Fail)
}

func TestCheckARPSanity(t *testing.T) {
	vip := netip.MustParseAddr("192.168.1.100")
	// Every node seeing the holder's MAC is the healthy case.
	clean := []VIPState{{Addr: vip, ARPMACs: map[string]string{"n1": "aa:aa:aa:aa:aa:01", "n2": "aa:aa:aa:aa:aa:01"}}}
	status(t, checkARPSanity(clean), Pass)
	// Distinct MACs for one VIP = two nodes both owning it.
	dup := []VIPState{{Addr: vip, ARPMACs: map[string]string{"n1": "aa:aa:aa:aa:aa:01", "n2": "bb:bb:bb:bb:bb:02"}}}
	status(t, checkARPSanity(dup), Fail)
}

func TestCheckBlockDNS(t *testing.T) {
	status(t, checkBlockDNS([]DNSProbe{{Node: "self", Name: "web.default.svc"}}), Pass)
	status(t, checkBlockDNS([]DNSProbe{{Node: "self", Name: "web", Err: "no such host"}}), Fail)
	status(t, checkBlockDNS(nil), Warn)
}

func TestCheckUpstreamDNS(t *testing.T) {
	status(t, checkUpstreamDNS(""), Pass)
	status(t, checkUpstreamDNS("upstream DNS unreachable: 1.1.1.1"), Warn)
}

func TestCheckFirewall(t *testing.T) {
	r := status(t, checkFirewall(true, map[string]uint64{"input": 42}, ""), Pass)
	if !strings.Contains(r.Detail, "input=42") {
		t.Fatalf("detail %q missing drop counter", r.Detail)
	}
	status(t, checkFirewall(false, nil, ""), Fail)
	status(t, checkFirewall(false, nil, "agent not running"), Warn)
}

func TestCheckConntrack(t *testing.T) {
	status(t, checkConntrack(10, 1000), Pass)
	status(t, checkConntrack(750, 1000), Warn)
	status(t, checkConntrack(950, 1000), Fail)
	status(t, checkConntrack(10, 0), Warn)
}

func TestCheckTimeSync(t *testing.T) {
	good := []NodeOffset{{Node: "self", Offset: 2 * time.Millisecond}}
	status(t, checkTimeSync(good), Pass)
	status(t, checkTimeSync(nil), Pass)
	bad := []NodeOffset{{Node: "n2", Offset: 250 * time.Millisecond}}
	status(t, checkTimeSync(bad), Fail)
	errd := []NodeOffset{{Node: "n3", Err: "chrony not installed"}}
	status(t, checkTimeSync(errd), Fail)
}

func TestFormatGolden(t *testing.T) {
	rs := []Result{
		{Check: "interfaces", Status: Pass, Detail: "3 links up"},
		{
			Check: "exp0", Status: Fail, Detail: "MTU 1500, want 1420",
			Hint: "ip link set exp0 mtu 1420",
		},
		{
			Check: "vips", Status: Warn, Detail: "192.168.1.100 unheld",
			Hint: "check lease holder liveness",
		},
	}
	golden := Format(rs)
	want := strings.Join([]string{
		"CHECK       STAT  DETAIL",
		"interfaces  PASS  3 links up",
		"exp0        FAIL  MTU 1500, want 1420",
		"            ↳ ip link set exp0 mtu 1420",
		"vips        WARN  192.168.1.100 unheld",
		"            ↳ check lease holder liveness",
		"",
	}, "\n")
	if golden != want {
		t.Fatalf("golden mismatch:\n%s\nwant:\n%s", golden, want)
	}
}

// Run calls all 12 checks in §5 order.
func TestRunOrder(t *testing.T) {
	rs := Run(Input{})
	want := []string{
		"interfaces", "exp0", "overlay-peers", "df-mtu", "port-matrix",
		"vips", "arp", "block-dns", "upstream-dns", "firewall",
		"conntrack", "time-sync",
	}
	if len(rs) != len(want) {
		t.Fatalf("got %d checks, want %d", len(rs), len(want))
	}
	for i, r := range rs {
		if r.Check != want[i] {
			t.Fatalf("check %d = %s, want %s", i, r.Check, want[i])
		}
	}
}
