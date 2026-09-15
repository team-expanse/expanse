package mesh

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/store/boltstore"
	"github.com/expanse/expanse/proto"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	pbproto "google.golang.org/protobuf/proto"
)

func newTestStore(t *testing.T) store.Store {
	t.Helper()
	st, err := boltstore.New(filepath.Join(t.TempDir(), "bolt.db"))
	if err != nil {
		t.Fatalf("boltstore.New: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// T02: keypair generated once, stable across restarts; private key 0600;
// public key published and republished idempotently.
func TestEnsureIdentityGenerateOnceStable(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "network", "wg.key")

	// First start: generates and publishes.
	st := newTestStore(t)
	id1, err := EnsureIdentity(context.Background(), st, "n1", keyFile, "192.168.1.1:51820")
	if err != nil {
		t.Fatalf("EnsureIdentity (generate): %v", err)
	}

	// Private key file: 0600, and only the private key lives there.
	fi, err := os.Stat(keyFile)
	if err != nil {
		t.Fatalf("stat key file: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("key file perm = %v, want 0600", fi.Mode().Perm())
	}

	// Published record matches the generated key.
	ent, err := st.Get(context.Background(), PublicKeyKey("n1"))
	if err != nil {
		t.Fatalf("Get published key: %v", err)
	}
	var peer proto.WireGuardPeer
	if err := pbproto.Unmarshal(ent.Value, &peer); err != nil {
		t.Fatalf("Unmarshal published peer: %v", err)
	}
	if peer.PublicKey != id1.Private.PublicKey().String() {
		t.Errorf("published key mismatch: %q != %q", peer.PublicKey, id1.Private.PublicKey().String())
	}
	if peer.OverlayPrefix != "10.42.1.0/24" {
		t.Errorf("OverlayPrefix = %q, want 10.42.1.0/24", peer.OverlayPrefix)
	}
	if peer.NodeId != "n1" {
		t.Errorf("NodeId = %q, want n1", peer.NodeId)
	}

	// Restart with a fresh store view (simulating a re-join against an
	// existing record): same key file → same public key, no rotation,
	// publish is idempotent.
	id2, err := EnsureIdentity(context.Background(), st, "n1", keyFile, "192.168.1.1:51820")
	if err != nil {
		t.Fatalf("EnsureIdentity (restart): %v", err)
	}
	if id2.Private.PublicKey() != id1.Private.PublicKey() {
		t.Error("key rotated across restart — must be stable for the node's lifetime")
	}
}

func TestEnsureIdentityRejectsKeyMismatch(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// Node publishes under key file A...
	dirA := t.TempDir()
	if _, err := EnsureIdentity(ctx, st, "n1", filepath.Join(dirA, "wg.key"), "192.168.1.1:51820"); err != nil {
		t.Fatalf("EnsureIdentity A: %v", err)
	}

	// ...then a different key file (simulated regenerated key / lost
	// private key) must hard-fail, not silently overwrite the record.
	dirB := t.TempDir()
	_, err := EnsureIdentity(ctx, st, "n1", filepath.Join(dirB, "wg.key"), "192.168.1.1:51820")
	if err == nil {
		t.Fatal("expected conflict error for mismatched published key, got nil")
	}
}

// Index claim: second node gets the next index; same node is stable.
func TestClaimIndexStableAndSequential(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	for i, node := range []string{"n1", "n2", "n3"} {
		idx, err := ClaimIndex(ctx, st, node)
		if err != nil {
			t.Fatalf("ClaimIndex(%s): %v", node, err)
		}
		if idx != i+1 {
			t.Errorf("ClaimIndex(%s) = %d, want %d", node, idx, i+1)
		}
	}
	if idx, _ := ClaimIndex(ctx, st, "n2"); idx != 2 {
		t.Errorf("re-claim for n2 = %d, want 2", idx)
	}
}

// Concurrent starters racing on the same key file must converge on one key.
func TestEnsureIdentityConvergesUnderRace(t *testing.T) {
	st := newTestStore(t)
	keyFile := filepath.Join(t.TempDir(), "network", "wg.key")

	const racers = 8
	errs := make(chan error, racers)
	for i := 0; i < racers; i++ {
		go func() {
			id, err := EnsureIdentity(context.Background(), st, "n1", keyFile, "192.168.1.1:51820")
			if err == nil && id.Peer.PublicKey == "" {
				err = os.ErrInvalid // empty key published
			}
			errs <- err
		}()
	}
	for i := 0; i < racers; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("racer: %v", err)
		}
	}

	// Exactly one key ended up on disk and in the store, consistently:
	// the disk file holds the private key; the published record must be
	// its public half.
	b, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatalf("read key: %v", err)
	}
	priv, err := wgtypes.ParseKey(string(b))
	if err != nil {
		t.Fatalf("parse disk key: %v", err)
	}
	ent, err := st.Get(context.Background(), PublicKeyKey("n1"))
	if err != nil {
		t.Fatalf("get published: %v", err)
	}
	var peer proto.WireGuardPeer
	if err := pbproto.Unmarshal(ent.Value, &peer); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if priv.PublicKey().String() != peer.PublicKey {
		t.Errorf("disk private key's public half %q != published %q", priv.PublicKey().String(), peer.PublicKey)
	}
}
