// Exported helpers for the Phase 05 chaos extensions
// (test/chaos/network): scenario timing, node IDs, and WireGuard peer
// records for the key-rotation scenario.
package chaos

import (
	"fmt"
	"os"
	"time"

	"github.com/expanse/expanse/internal/store"

	"github.com/expanse/expanse/internal/network/addrplan"
	"github.com/expanse/expanse/proto"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	pbproto "google.golang.org/protobuf/proto"
)

// NodeID returns node i's cluster ID ("n<i>").
func (h *Harness) NodeID(i int) string { return h.nodeID(i) }

// scenarioDuration returns the length of one chaos scenario. Default 20 s
// (fast suite); RUN_CHAOS=1 scales to the spec's 5 minutes per scenario.
func scenarioDuration() time.Duration {
	if os.Getenv("RUN_CHAOS") == "1" {
		return 5 * time.Minute
	}
	if d, err := time.ParseDuration(os.Getenv("CHAOS_DURATION")); err == nil && d > 0 {
		return d
	}
	return 20 * time.Second
}

// faultInterval scales a fault cadence to the scenario duration: at the
// full 5 minutes the intervals match the spec table (kills every 10–60 s,
// partitions every 20–90 s, transfers every 15 s); short runs compress
// the same number of fault events.
func faultInterval(scenario time.Duration, min, max time.Duration) time.Duration {
	d := scenario / 12
	if d < 300*time.Millisecond {
		d = 300 * time.Millisecond
	}
	if d > max {
		d = max
	}
	if d < min {
		d = min
	}
	return d
}

// PeerRecord returns node i's WireGuardPeer record (generated on first
// call, stable afterwards): real keypair, §3 overlay prefix
// 10.42.<i+1>.0/24, endpoint = its raft addr.
func (h *Harness) PeerRecord(i int) *proto.WireGuardPeer {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.peerKeys == nil {
		h.peerKeys = make(map[int]wgtypes.Key)
	}
	priv, ok := h.peerKeys[i]
	if !ok {
		k, err := wgtypes.GeneratePrivateKey()
		if err != nil {
			h.t.Fatalf("generate key n%d: %v", i, err)
		}
		h.peerKeys[i] = k
		priv = k
	}
	prefix, err := addrplan.OverlayPrefix(i + 1)
	if err != nil {
		h.t.Fatalf("overlay prefix n%d: %v", i, err)
	}
	return &proto.WireGuardPeer{
		NodeId:        h.nodeID(i),
		PublicKey:     priv.PublicKey().String(),
		OverlayPrefix: prefix.String(),
		Endpoint:      h.raftAddr(i),
	}
}

// RotatePeerKey replaces node i's private key (the §6 key-rotation
// fault). The new record only reaches the store when the scenario
// republishes via PeerRecord + Put.
func (h *Harness) RotatePeerKey(i int, privBase64 string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	priv, err := wgtypes.ParseKey(privBase64)
	if err != nil {
		h.t.Fatalf("parse rotated key n%d: %v", i, err)
	}
	if h.peerKeys == nil {
		h.peerKeys = make(map[int]wgtypes.Key)
	}
	h.peerKeys[i] = priv
}

// MarshalPeer serializes a peer record for store writes.
func MarshalPeer(p *proto.WireGuardPeer) ([]byte, error) {
	return pbproto.Marshal(p)
}

// ScenarioDuration exposes the scenario budget (RUN_CHAOS=1 → 5 m,
// CHAOS_DURATION override, default 20 s).
func ScenarioDuration() time.Duration { return scenarioDuration() }

// FaultInterval exposes the compressed fault cadence.
func FaultInterval(scenario, min, max time.Duration) time.Duration {
	return faultInterval(scenario, min, max)
}

// LoadKey returns a unique write key for the sustained-load scenario.
func LoadKey(worker, seq int) store.Key {
	return store.Key(fmt.Sprintf("/chaos-load/w%d/k%d", worker, seq))
}
