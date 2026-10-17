package vip

import (
	"net"
	"testing"

	"github.com/vishvananda/netlink"
)

func TestIsDefaultRoute(t *testing.T) {
	_, zero, _ := net.ParseCIDR("0.0.0.0/0")
	_, lan, _ := net.ParseCIDR("192.168.3.0/24")
	_, host, _ := net.ParseCIDR("0.0.0.0/32")
	for _, tc := range []struct {
		name  string
		route netlink.Route
		want  bool
	}{
		{"netlink v1.3 default (0.0.0.0/0)", netlink.Route{Dst: zero, LinkIndex: 3}, true},
		{"older netlink default (nil Dst)", netlink.Route{LinkIndex: 3}, true},
		{"subnet route", netlink.Route{Dst: lan, LinkIndex: 3}, false},
		{"0.0.0.0/32 is not default", netlink.Route{Dst: host, LinkIndex: 3}, false},
		{"default with no link", netlink.Route{Dst: zero}, false},
	} {
		if got := IsDefaultRoute(tc.route); got != tc.want {
			t.Errorf("%s: IsDefaultRoute = %v, want %v", tc.name, got, tc.want)
		}
	}
}
