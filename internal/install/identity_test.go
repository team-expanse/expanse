package install

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEnsureIdentityCreatesValidIdentity(t *testing.T) {
	dir := t.TempDir()
	iddir := filepath.Join(dir, "identity")

	id, err := EnsureIdentity(iddir)
	if err != nil {
		t.Fatalf("EnsureIdentity: %v", err)
	}
	if id.NodeID.String() == "" {
		t.Fatal("empty node id")
	}
	if len(id.PublicKey) != ed25519.PublicKeySize {
		t.Fatalf("bad public key size %d", len(id.PublicKey))
	}
	if id.Created.IsZero() {
		t.Fatal("created timestamp not set")
	}

	// Modes per spec.
	fi, err := os.Stat(filepath.Join(iddir, "node-id"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("node-id mode = %o, want 0644", fi.Mode().Perm())
	}
	fi, err = os.Stat(filepath.Join(iddir, "node.key"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("node.key mode = %o, want 0600", fi.Mode().Perm())
	}

	// created file parses as RFC3339.
	c, err := os.ReadFile(filepath.Join(iddir, "created"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := time.Parse(time.RFC3339, string(c[:len(c)-1])); err != nil {
		t.Errorf("created not RFC3339: %v", err)
	}
}

func TestEnsureIdentityIdempotent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "identity")
	id1, err := EnsureIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := EnsureIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	if id1.NodeID != id2.NodeID {
		t.Errorf("node id changed: %s -> %s", id1.NodeID, id2.NodeID)
	}
	if !id1.PublicKey.Equal(id2.PublicKey) {
		t.Error("public key changed on second run")
	}
}

func TestLoadIdentity(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "identity")
	id1, err := EnsureIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := LoadIdentity(dir)
	if err != nil {
		t.Fatalf("LoadIdentity: %v", err)
	}
	if id1.NodeID != id2.NodeID {
		t.Errorf("loaded node id mismatch: %s != %s", id1.NodeID, id2.NodeID)
	}
	if !id1.PublicKey.Equal(id2.PublicKey) {
		t.Error("loaded public key mismatch")
	}
}

func TestSignatureVerifies(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "identity")
	_, err := EnsureIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Sign with the private key from disk, verify with the public key.
	priv, err := os.ReadFile(filepath.Join(dir, "node.key"))
	if err != nil {
		t.Fatal(err)
	}
	pub, err := os.ReadFile(filepath.Join(dir, "node.pub"))
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("expanse identity test")
	sig := ed25519.Sign(ed25519.PrivateKey(priv), msg)
	if !ed25519.Verify(ed25519.PublicKey(pub), msg, sig) {
		t.Fatal("signature made with node.key does not verify against node.pub")
	}
}

func TestLoadIdentityRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "node-id"), []byte("not-a-uuid\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureIdentity(dir); err == nil {
		t.Error("EnsureIdentity should reject corrupt node-id")
	}
	if _, err := LoadIdentity(dir); err == nil {
		t.Error("LoadIdentity should reject corrupt node-id")
	}
}

func TestHostIDAndHostname(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "identity")
	id, err := EnsureIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := id.HostID(); len(got) != 8 {
		t.Errorf("HostID length = %d, want 8", len(got))
	}
	hn := id.DefaultHostname()
	if len(hn) != len("expanse-xxxxxx") {
		t.Errorf("DefaultHostname = %q", hn)
	}
}
