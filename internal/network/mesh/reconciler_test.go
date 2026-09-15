package mesh

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/boltstore"
	"github.com/expanse/expanse/proto"
	pbproto "google.golang.org/protobuf/proto"
)

// --- fake controller -----------------------------------------------------

type fakeCtrl struct {
	mu        sync.Mutex
	deviceMTU int
	addrs     []netip.Addr         // assigned overlay addresses
	peers     map[string]PeerState // by public key
	ops       []string             // applied-op log for incremental assertions
	failNext  string               // op kind to fail once
}

func newFakeCtrl() *fakeCtrl {
	return &fakeCtrl{peers: make(map[string]PeerState)}
}

func (f *fakeCtrl) EnsureDevice(mtu int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deviceMTU == 0 {
		f.ops = append(f.ops, fmt.Sprintf("create mtu=%d", mtu))
	} else if f.deviceMTU != mtu {
		f.ops = append(f.ops, fmt.Sprintf("mtu %d->%d", f.deviceMTU, mtu))
	}
	f.deviceMTU = mtu
	return nil
}

func (f *fakeCtrl) EnsureAddr(a netip.Addr) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, x := range f.addrs {
		if x == a {
			return nil
		}
	}
	f.addrs = append(f.addrs, a)
	f.ops = append(f.ops, "addr "+a.String())
	return nil
}

func (f *fakeCtrl) Peers() ([]PeerState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]PeerState, 0, len(f.peers))
	for _, p := range f.peers {
		out = append(out, p)
	}
	return out, nil
}

func (f *fakeCtrl) SetPeer(spec PeerSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext == "set" {
		f.failNext = ""
		return fmt.Errorf("injected set failure")
	}
	_, existed := f.peers[spec.PublicKey]
	f.peers[spec.PublicKey] = PeerState{
		PublicKey:  spec.PublicKey,
		Endpoint:   spec.Endpoint,
		AllowedIPs: spec.AllowedIPs,
	}
	kind := "add"
	if existed {
		kind = "update"
	}
	f.ops = append(f.ops, fmt.Sprintf("%s %s ips=%d", kind, spec.NodeID, len(spec.AllowedIPs)))
	return nil
}

func (f *fakeCtrl) RemovePeer(publicKey string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.peers, publicKey)
	f.ops = append(f.ops, "remove "+publicKey)
	return nil
}

// --- store helpers -------------------------------------------------------

type meshFixture struct {
	st  store.Store
	rec *Reconciler
}

func newFixture(t *testing.T, mtu int) *meshFixture {
	t.Helper()
	st, err := boltstore.New(t.TempDir() + "/bolt.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctrl := newFakeCtrl()
	rec := NewReconciler(st, ctrl, Config{PhysicalMTU: mtu, SelfNodeID: "n1"})
	return &meshFixture{st: st, rec: rec}
}

func (f *meshFixture) publish(t *testing.T, nodeID, pubkey string, idx int) {
	t.Helper()
	p := &proto.WireGuardPeer{
		NodeId:        nodeID,
		PublicKey:     pubkey,
		OverlayPrefix: fmt.Sprintf("10.42.%d.0/24", idx),
	}
	b, err := pbproto.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Put(context.Background(), PublicKeyKey(nodeID), b); err != nil {
		t.Fatal(err)
	}
}

func (f *meshFixture) setHolder(t *testing.T, vip, holder string) {
	t.Helper()
	if _, err := f.st.Put(context.Background(), store.Key("/network/vips/"+vip+"/holder"), []byte(holder)); err != nil {
		t.Fatal(err)
	}
}

func pfx(s string) netip.Prefix {
	p, err := netip.ParsePrefix(s)
	if err != nil {
		panic(err)
	}
	return p
}

// --- tests ----------------------------------------------------------------

// The core card deliverable: diff produces the exact incremental ops.
func TestDiffAddUpdateRemove(t *testing.T) {
	state := []PeerState{
		{PublicKey: "aaa", Endpoint: "10.0.0.1:51820", AllowedIPs: []netip.Prefix{pfx("10.42.1.0/24")}},
		{PublicKey: "bbb", Endpoint: "10.0.0.2:51820", AllowedIPs: []netip.Prefix{pfx("10.42.2.0/24")}},
		{PublicKey: "ccc", Endpoint: "10.0.0.3:51820", AllowedIPs: []netip.Prefix{pfx("10.42.3.0/24")}},
	}
	specs := []PeerSpec{
		// aaa: unchanged → no op
		{PublicKey: "aaa", Endpoint: "10.0.0.1:51820", AllowedIPs: []netip.Prefix{pfx("10.42.1.0/24")}},
		// bbb: gained a VIP → update
		{PublicKey: "bbb", Endpoint: "10.0.0.2:51820", AllowedIPs: []netip.Prefix{pfx("10.42.2.0/24"), pfx("192.168.1.100/32")}},
		// ddd: new → add
		{PublicKey: "ddd", Endpoint: "10.0.0.4:51820", AllowedIPs: []netip.Prefix{pfx("10.42.4.0/24")}},
		// ccc: gone → remove
	}

	ops := Diff(state, specs)
	var got []string
	for _, op := range ops {
		switch op.Kind {
		case OpRemove:
			got = append(got, "remove:"+op.PublicKey)
		case OpAdd:
			got = append(got, "add:"+op.Spec.PublicKey)
		case OpUpdate:
			got = append(got, "update:"+op.Spec.PublicKey)
		}
	}
	want := []string{"remove:ccc", "update:bbb", "add:ddd"} // remove, update, add order
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("ops = %v, want %v", got, want)
	}
}

// AllowedIPs compare is set-based: reordering alone must not update.
func TestDiffOrderInsensitiveAllowedIPs(t *testing.T) {
	specs := []PeerSpec{{
		PublicKey: "aaa",
		AllowedIPs: []netip.Prefix{
			pfx("192.168.1.100/32"), pfx("10.42.1.0/24"), pfx("192.168.1.101/32"),
		},
	}}
	state := []PeerState{{
		PublicKey: "aaa",
		AllowedIPs: []netip.Prefix{
			pfx("10.42.1.0/24"), pfx("192.168.1.101/32"), pfx("192.168.1.100/32"),
		},
	}}
	ops := Diff(state, specs)
	if len(ops) != 0 {
		t.Errorf("reordered AllowedIPs should be a no-op, got %v", ops)
	}
}

// Endpoint change alone must update.
func TestDiffEndpointChange(t *testing.T) {
	ops := Diff(
		[]PeerState{{PublicKey: "aaa", Endpoint: "10.0.0.1:51820", AllowedIPs: []netip.Prefix{pfx("10.42.1.0/24")}}},
		[]PeerSpec{{PublicKey: "aaa", Endpoint: "10.0.0.9:51820", AllowedIPs: []netip.Prefix{pfx("10.42.1.0/24")}}},
	)
	if len(ops) != 1 || ops[0].Kind != OpUpdate {
		t.Errorf("want single update, got %v", ops)
	}
}

// Full reconcile pass: publish 3 nodes (one is self) → device converges
// to 2 peers with the right AllowedIPs; VIP holders fold in as /32s.
func TestReconcileConverges(t *testing.T) {
	f := newFixture(t, 1500)
	ctx := context.Background()

	f.publish(t, "n1", "KEY1", 1)
	f.publish(t, "n2", "KEY2", 2)
	f.publish(t, "n3", "KEY3", 3)
	f.setHolder(t, "192.168.1.100", "n2")
	f.setHolder(t, "10.43.0.10", "n3")

	if err := f.rec.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	ctrl := f.rec.ctrl.(*fakeCtrl)
	if len(ctrl.peers) != 2 {
		t.Fatalf("peers = %d, want 2 (self excluded)", len(ctrl.peers))
	}
	if p := ctrl.peers["KEY2"]; len(p.AllowedIPs) != 2 {
		t.Errorf("n2 AllowedIPs = %v, want overlay /24 + held VIP /32", p.AllowedIPs)
	}
	if p := ctrl.peers["KEY3"]; len(p.AllowedIPs) != 2 {
		t.Errorf("n3 AllowedIPs = %v, want overlay /24 + internal VIP /32", p.AllowedIPs)
	}
	if ctrl.deviceMTU != 1420 { // 1500 physical − 80
		t.Errorf("MTU = %d, want 1420", ctrl.deviceMTU)
	}
	if len(ctrl.addrs) != 1 || ctrl.addrs[0].String() != "10.42.1.1" {
		t.Errorf("addrs = %v, want [10.42.1.1]", ctrl.addrs)
	}

	// Idempotent: second pass is a no-op.
	before := len(ctrl.ops)
	if err := f.rec.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile #2: %v", err)
	}
	if len(ctrl.ops) != before {
		t.Errorf("second reconcile performed ops %v — must be a no-op", ctrl.ops[before:])
	}
}

// Mesh reforms: a node leaving (its record deleted) removes its peer;
// a new node's record adds it. Existing peers untouched (incremental).
func TestReconcileMeshReforms(t *testing.T) {
	f := newFixture(t, 1500)
	ctx := context.Background()

	f.publish(t, "n1", "KEY1", 1)
	f.publish(t, "n2", "KEY2", 2)
	if err := f.rec.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	ctrl := f.rec.ctrl.(*fakeCtrl)
	if got := ctrl.ops; len(got) != 3 || !strings.HasPrefix(got[0], "create") || !strings.Contains(got[0], "mtu=1420") || got[1] != "addr 10.42.1.1" || !strings.HasPrefix(got[2], "add") {
		t.Errorf("first pass ops = %v, want create mtu=1420 + addr + 1 add (self excluded)", got)
	}
	before := len(ctrl.ops)

	// n2 leaves.
	if err := f.st.Delete(ctx, PublicKeyKey("n2"), 0); err != nil {
		t.Fatal(err)
	}
	f.publish(t, "n4", "KEY4", 4)
	if err := f.rec.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := ctrl.peers["KEY2"]; ok {
		t.Error("departed node's peer still on device")
	}
	if _, ok := ctrl.peers["KEY4"]; !ok {
		t.Error("new node's peer missing")
	}

	// Only remove+add happened (plus no new create/MTU churn).
	newOps := ctrl.ops[before:]
	if len(newOps) != 2 || !strings.HasPrefix(newOps[0], "remove") || !strings.HasPrefix(newOps[1], "add") {
		t.Errorf("reform ops = %v, want exactly remove+add", newOps)
	}
}

// MTU rule table.
func TestMTURule(t *testing.T) {
	cases := []struct{ override, physical, want int }{
		{0, 1500, 1420},
		{1380, 1500, 1380}, // override wins
		{0, 9000, 8920},    // jumbo physical
		{0, 40, 1420},      // absurd physical → default
	}
	for _, c := range cases {
		if got := MTU(c.override, c.physical); got != c.want {
			t.Errorf("MTU(%d,%d) = %d, want %d", c.override, c.physical, got, c.want)
		}
	}
}
