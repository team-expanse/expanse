package device

import (
	"testing"

	"github.com/mdlayher/netlink"
)

func TestNBDDeviceIndex(t *testing.T) {
	for dev, want := range map[string]uint32{"/dev/nbd0": 0, "/dev/nbd12": 12} {
		got, err := nbdDeviceIndex(dev)
		if err != nil || got != want {
			t.Errorf("nbdDeviceIndex(%q) = %d, %v; want %d", dev, got, err, want)
		}
	}
	for _, bad := range []string{"", "/dev/sda", "/dev/nbd", "/dev/nbdx", "/dev/nbd-1"} {
		if _, err := nbdDeviceIndex(bad); err == nil {
			t.Errorf("nbdDeviceIndex(%q) accepted a non-nbd device", bad)
		}
	}
}

func TestNBDReconfigureSizeMessage(t *testing.T) {
	m, err := nbdReconfigureSizeMessage(3, 20<<30)
	if err != nil {
		t.Fatal(err)
	}
	if m.Header.Command != nbdCmdReconfigure || m.Header.Version != nbdGenlVersion {
		t.Fatalf("header = %+v", m.Header)
	}
	ad, err := netlink.NewAttributeDecoder(m.Data)
	if err != nil {
		t.Fatal(err)
	}
	// INDEX is a u32 attribute, SIZE_BYTES a u64 (nbd-netlink.h policy).
	var index uint32
	var size uint64
	n := 0
	for ad.Next() {
		n++
		switch ad.Type() {
		case nbdAttrIndex:
			index = ad.Uint32()
		case nbdAttrSizeBytes:
			size = ad.Uint64()
		}
	}
	if err := ad.Err(); err != nil {
		t.Fatal(err)
	}
	if index != 3 || size != 20<<30 || n != 2 {
		t.Fatalf("attrs: index=%d size=%d count=%d, want 3, %d, 2", index, size, n, uint64(20<<30))
	}
}
