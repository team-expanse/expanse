package discovery

import (
	"net"
	"testing"

	"github.com/hashicorp/mdns"
)

// TestRecordFromEntry covers the mdns-entry → Record conversion.
func TestRecordFromEntry(t *testing.T) {
	e := &mdns.ServiceEntry{
		Name:   "expanse._tcp.local.",
		Port:   8443,
		AddrV4: net.IPv4(192, 168, 1, 7),
		InfoFields: []string{
			"node_id=n7",
			"role=witness",
			"cluster_id=11111111-2222-3333-4444-555555555555",
		},
	}
	rec := recordFromEntry(e)
	if rec == nil {
		t.Fatal("recordFromEntry returned nil")
	}
	if rec.NodeID != "n7" || rec.Role != "witness" || rec.APIPort != 8443 {
		t.Errorf("record = %+v", rec)
	}
	if rec.Addr != "192.168.1.7" {
		t.Errorf("addr = %q", rec.Addr)
	}

	// No addresses: falls back to the instance name for NodeID.
	e2 := &mdns.ServiceEntry{Name: "n8." + ServiceName + ".local.", Port: 1, InfoFields: []string{"role=voter"}}
	if rec2 := recordFromEntry(e2); rec2 == nil || rec2.NodeID != "n8" {
		t.Errorf("name fallback record = %+v", rec2)
	}
}

// TestAdvertiseClose covers the advertiser lifecycle where mDNS can
// bind; environments without multicast skip.
func TestAdvertiseClose(t *testing.T) {
	adv, err := Advertise("11111111-2222-3333-4444-555555555555", "n-lc", "voter", 8443)
	if err != nil {
		t.Skipf("mDNS unavailable: %v", err)
	}
	if adv == nil {
		t.Fatal("nil advertiser")
	}
	if err := adv.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}
