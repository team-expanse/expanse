package agent

import (
	"net/netip"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/network/vip"
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

// G5.4: a client recovers within 15 s of the VIP holder dying. Takeover waits
// out the lease, then one connect to the dead node's backend may time out.
func TestVIPFailoverFitsTheG54Budget(t *testing.T) {
	const electionAndAnnounce = 2 * time.Second // measured: takeover lands ≤ 0.6 s past expiry
	if worst := vip.LeaseTTL + electionAndAnnounce + proxy.DefaultDialTimeout; worst > 15*time.Second {
		t.Fatalf("worst-case failover %v exceeds G5.4's 15 s", worst)
	}
}
