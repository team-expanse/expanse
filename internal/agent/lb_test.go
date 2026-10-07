package agent

import (
	"net/netip"
	"testing"

	"github.com/expanse/expanse/internal/proxy"
)

func TestDialFromVIPOnlyForThisNodesBackends(t *testing.T) {
	vip := netip.MustParseAddr("192.168.1.101")
	from := dialFromVIP("n1", vip)
	if got := from(proxy.Backend{NodeID: "n1"}); got != vip {
		t.Errorf("local backend dialed from %v, want the VIP", got)
	}
	if got := from(proxy.Backend{NodeID: "n2"}); got.IsValid() {
		t.Errorf("remote backend dialed from %v, want the kernel's choice", got)
	}
}
