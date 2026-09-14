package addrplan

import (
	"net/netip"
	"testing"
)

func TestOverlayPrefix(t *testing.T) {
	tests := []struct {
		node int
		want string
	}{
		{1, "10.42.1.0/24"},
		{2, "10.42.2.0/24"},
		{3, "10.42.3.0/24"},
		{16, "10.42.16.0/24"},
		{254, "10.42.254.0/24"},
	}
	for _, tt := range tests {
		got, err := OverlayPrefix(tt.node)
		if err != nil {
			t.Fatalf("OverlayPrefix(%d): %v", tt.node, err)
		}
		if got.String() != tt.want {
			t.Errorf("OverlayPrefix(%d) = %s, want %s", tt.node, got, tt.want)
		}
	}
}

func TestOverlayPrefixInvalid(t *testing.T) {
	for _, n := range []int{-1, 0, 255, 1000} {
		if _, err := OverlayPrefix(n); err == nil {
			t.Errorf("OverlayPrefix(%d): expected error, got nil", n)
		}
	}
}

func TestOverlayAddress(t *testing.T) {
	tests := []struct {
		node int
		want netip.Addr
	}{
		{1, netip.AddrFrom4([4]byte{10, 42, 1, 1})},
		{3, netip.AddrFrom4([4]byte{10, 42, 3, 1})},
	}
	for _, tt := range tests {
		got, err := OverlayAddress(tt.node)
		if err != nil {
			t.Fatalf("OverlayAddress(%d): %v", tt.node, err)
		}
		if got != tt.want {
			t.Errorf("OverlayAddress(%d) = %s, want %s", tt.node, got, tt.want)
		}
	}
}

func TestOverlayAddressInvalid(t *testing.T) {
	if _, err := OverlayAddress(255); err == nil {
		t.Error("OverlayAddress(255): expected error, got nil")
	}
}
