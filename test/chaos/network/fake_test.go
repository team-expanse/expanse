package network

import (
	"net/netip"
	"sync"

	"github.com/expanse/expanse/internal/network/mesh"
)

// fakeCtrl is a minimal mesh.Controller (the reconciler test's fake,
// copied down because that one is package-private): records applied
// peers so the key-rotation scenario can assert convergence.
type fakeCtrl struct {
	mu    sync.Mutex
	peers map[string]mesh.PeerState // by public key
}

func newFakeCtrl() *fakeCtrl { return &fakeCtrl{peers: make(map[string]mesh.PeerState)} }

func (f *fakeCtrl) EnsureDevice(mtu int) error    { return nil }
func (f *fakeCtrl) EnsureAddr(a netip.Addr) error { return nil }

func (f *fakeCtrl) Peers() ([]mesh.PeerState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]mesh.PeerState, 0, len(f.peers))
	for _, p := range f.peers {
		out = append(out, p)
	}
	return out, nil
}

func (f *fakeCtrl) SetPeer(spec mesh.PeerSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.peers[spec.PublicKey] = mesh.PeerState{
		PublicKey:  spec.PublicKey,
		Endpoint:   spec.Endpoint,
		AllowedIPs: spec.AllowedIPs,
	}
	return nil
}

func (f *fakeCtrl) RemovePeer(publicKey string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.peers, publicKey)
	return nil
}

// HasPeer reports whether the controller currently holds the key.
func (f *fakeCtrl) HasPeer(publicKey string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.peers[publicKey]
	return ok
}
