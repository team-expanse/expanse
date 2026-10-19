package vm

import (
	"net"
	"testing"
)

func TestDefaultMAC(t *testing.T) {
	// Pinned: a changed derivation would give running VMs a new address.
	if got := DefaultMAC("default-vm1-0"); got != "7a:81:6e:cd:39:8d" {
		t.Errorf("DefaultMAC = %s", got)
	}
	if DefaultMAC("default-other-0") == DefaultMAC("default-vm1-0") {
		t.Error("two instances share a MAC")
	}
	mac, err := net.ParseMAC(DefaultMAC("default-vm1-0"))
	if err != nil || mac[0]&0x01 != 0 || mac[0]&0x02 == 0 {
		t.Errorf("%v %v: want locally administered unicast", mac, err)
	}
}

func TestMACPrefersConfig(t *testing.T) {
	if got := MAC(map[string]any{"mac": "52:54:00:00:00:01"}, "default-vm1-0"); got != "52:54:00:00:00:01" {
		t.Errorf("MAC = %s", got)
	}
	if got := MAC(nil, "default-vm1-0"); got != DefaultMAC("default-vm1-0") {
		t.Errorf("MAC = %s", got)
	}
}
