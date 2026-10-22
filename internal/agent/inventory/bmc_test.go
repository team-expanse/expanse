package inventory

import (
	"errors"
	"path/filepath"
	"testing"
)

// idracLanPrint is `ipmitool lan print` from a Dell iDRAC, trimmed.
const idracLanPrint = `Set in Progress         : Set Complete
Auth Type Support       : 
IP Address Source       : DHCP Address
IP Address              : 10.0.40.17
Subnet Mask             : 255.255.255.0
MAC Address             : d0:94:66:1a:2b:3c
SNMP Community String   : public
Default Gateway IP      : 10.0.40.1
802.1q VLAN ID          : Disabled
Cipher Suite Priv Max   : Xaaaaaaaaaaaaaa
                        :     X=Cipher Suite Unused
`

func TestLanPrintGivesTheBMCsAddress(t *testing.T) {
	got := parseLanPrint(idracLanPrint)
	want := BMCInfo{IP: "10.0.40.17", MAC: "d0:94:66:1a:2b:3c", Source: "DHCP Address"}
	if got == nil || *got != want {
		t.Fatalf("parseLanPrint = %+v, want %+v", got, want)
	}
}

func TestAnUnconfiguredBMCKeepsItsMAC(t *testing.T) {
	got := parseLanPrint("IP Address              : 0.0.0.0\nMAC Address             : d0:94:66:1a:2b:3c\n")
	if got == nil || got.IP != "" || got.MAC != "d0:94:66:1a:2b:3c" {
		t.Fatalf("parseLanPrint = %+v, want MAC only", got)
	}
}

func TestNoLanChannelIsNoBMC(t *testing.T) {
	if got := parseLanPrint("Invalid channel: 1\n"); got != nil {
		t.Fatalf("parseLanPrint = %+v, want nil", got)
	}
}

func TestCollectReadsTheBMCOnlyWhenTheHostHasOne(t *testing.T) {
	f := newFixture(t)
	calls := 0
	c := Collector{Roots: f.root, LanPrint: func() (string, error) { calls++; return idracLanPrint, nil }}
	if inv, _ := c.Collect("n"); inv.BMC != nil || calls != 0 {
		t.Fatalf("no /dev/ipmi0: BMC = %+v after %d calls, want nil and none", inv.BMC, calls)
	}

	writeFile(t, filepath.Join(f.root.Dev, "ipmi0"), "")
	if inv, _ := c.Collect("n"); inv.BMC == nil || inv.BMC.IP != "10.0.40.17" {
		t.Fatalf("BMC = %+v, want 10.0.40.17", inv.BMC)
	}

	c.LanPrint = func() (string, error) { return idracLanPrint, errors.New("exit status 1") }
	if inv, _ := c.Collect("n"); inv.BMC == nil {
		t.Fatal("lan print exiting 1 after printing: BMC = nil, want its address")
	}

	c.LanPrint = func() (string, error) { return "", errors.New("timed out") }
	if inv, _ := c.Collect("n"); inv.BMC != nil {
		t.Fatalf("failed lan print: BMC = %+v, want nil", inv.BMC)
	}
}
