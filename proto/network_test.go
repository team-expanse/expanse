// Round-trip coverage for proto/network.proto (Phase 05 T01).
//
// These messages are store-marshaled values (mesh peers, VIP records,
// pool config); later cards (mesh reconciler, VIP holder, DNS zone
// builder) rely on every field round-tripping exactly.
package proto

import (
	"testing"

	pbproto "google.golang.org/protobuf/proto"
)

func TestWireGuardPeerRoundTrip(t *testing.T) {
	in := &WireGuardPeer{
		NodeId:        "n1",
		PublicKey:     "base64pubkey==",
		Endpoint:      "192.168.1.11:51820",
		OverlayPrefix: "10.42.1.0/24",
	}
	out := &WireGuardPeer{}
	b, err := pbproto.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := pbproto.Unmarshal(b, out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !pbproto.Equal(in, out) {
		t.Errorf("round-trip mismatch:\n in  = %+v\n out = %+v", in, out)
	}
}

func TestVIPRoundTrip(t *testing.T) {
	in := &VIP{
		Address:   "192.168.1.100/24",
		Interface: "eth0",
		Scope:     VIPScope_VIP_SCOPE_EXTERNAL,
		BlockRef:  "default/web",
		Holder:    "n2",
	}
	out := &VIP{}
	b, err := pbproto.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := pbproto.Unmarshal(b, out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !pbproto.Equal(in, out) {
		t.Errorf("round-trip mismatch:\n in  = %+v\n out = %+v", in, out)
	}
	if out.Scope != VIPScope_VIP_SCOPE_EXTERNAL {
		t.Errorf("Scope = %v, want EXTERNAL", out.Scope)
	}
}

func TestVIPPoolConfigRoundTrip(t *testing.T) {
	in := &VIPPoolConfig{
		Range:             "192.168.1.100-192.168.1.120",
		ExternalInterface: "eth0",
	}
	out := &VIPPoolConfig{}
	b, err := pbproto.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := pbproto.Unmarshal(b, out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !pbproto.Equal(in, out) {
		t.Errorf("round-trip mismatch:\n in  = %+v\n out = %+v", in, out)
	}
}

func TestVIPScopeEnumStrings(t *testing.T) {
	tests := []struct {
		s    VIPScope
		want string
	}{
		{VIPScope_VIP_SCOPE_UNSPECIFIED, "VIP_SCOPE_UNSPECIFIED"},
		{VIPScope_VIP_SCOPE_INTERNAL, "VIP_SCOPE_INTERNAL"},
		{VIPScope_VIP_SCOPE_EXTERNAL, "VIP_SCOPE_EXTERNAL"},
	}
	for _, tt := range tests {
		if got := tt.s.String(); got != tt.want {
			t.Errorf("VIPScope(%d).String() = %q, want %q", tt.s, got, tt.want)
		}
	}
}
