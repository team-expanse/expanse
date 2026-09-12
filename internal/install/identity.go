// Package install implements node identity creation and management.
package install

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Identity holds a node's stable identity.
type Identity struct {
	NodeID    uuid.UUID // UUIDv4
	PublicKey ed25519.PublicKey
	Created   time.Time
}

// IdentityDir is the path of the identity files relative to the persist dir.
const IdentityDir = "identity"

// IdentityPaths returns the paths of each identity file within dir.
type IdentityPaths struct {
	NodeID string
	Priv   string
	Pub    string
	Creatd string
}

// identityPaths returns file paths for the identity directory.
func identityPaths(dir string) IdentityPaths {
	return IdentityPaths{
		NodeID: filepath.Join(dir, "node-id"),
		Priv:   filepath.Join(dir, "node.key"),
		Pub:    filepath.Join(dir, "node.pub"),
		Creatd: filepath.Join(dir, "created"),
	}
}

// EnsureIdentity creates the identity in dir if it does not exist yet.
// It is idempotent: if node-id already exists and is valid, nothing is
// written and the existing identity is returned.
func EnsureIdentity(dir string) (*Identity, error) {
	p := identityPaths(dir)

	if data, err := os.ReadFile(p.NodeID); err == nil {
		if _, err := uuid.Parse(strings.TrimSpace(string(data))); err != nil {
			return nil, fmt.Errorf("identity dir %s: existing node-id is not a UUID: %w", dir, err)
		}
		return LoadIdentity(dir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", p.NodeID, err)
	}

	// Generate a fresh identity. Keys come from crypto/rand; never derive
	// from MAC address or hostname.
	id, err := uuid.NewRandom()
	if err != nil {
		return nil, fmt.Errorf("generate node id: %w", err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate keypair: %w", err)
	}
	created := time.Now().UTC()

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}

	files := []struct {
		path string
		data []byte
		mode os.FileMode
	}{
		{p.NodeID, []byte(id.String() + "\n"), 0o644},
		{p.Priv, priv, 0o600},
		{p.Pub, pub, 0o644},
		{p.Creatd, []byte(created.Format(time.RFC3339) + "\n"), 0o644},
	}
	for _, f := range files {
		if err := os.WriteFile(f.path, f.data, f.mode); err != nil {
			return nil, fmt.Errorf("write %s: %w", f.path, err)
		}
		if err := os.Chmod(f.path, f.mode); err != nil {
			return nil, fmt.Errorf("chmod %s: %w", f.path, err)
		}
	}

	return &Identity{NodeID: id, PublicKey: pub, Created: created}, nil
}

// LoadIdentity reads an existing identity from dir.
func LoadIdentity(dir string) (*Identity, error) {
	p := identityPaths(dir)

	idStr, err := os.ReadFile(p.NodeID)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p.NodeID, err)
	}
	id, err := uuid.Parse(strings.TrimSpace(string(idStr)))
	if err != nil {
		return nil, fmt.Errorf("parse node-id: %w", err)
	}

	pub, err := os.ReadFile(p.Pub)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p.Pub, err)
	}
	if len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("node.pub: want %d bytes, got %d", ed25519.PublicKeySize, len(pub))
	}

	created := time.Time{}
	if c, err := os.ReadFile(p.Creatd); err == nil {
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(c))); err == nil {
			created = t
		}
	}

	return &Identity{NodeID: id, PublicKey: ed25519.PublicKey(pub), Created: created}, nil
}

// HostID returns the ZFS networking.hostId derived from the node ID:
// the first 8 hex characters.
func (i *Identity) HostID() string {
	return i.NodeID.String()[:8]
}

// DefaultHostname returns expanse-<first 6 of node-id>.
func (i *Identity) DefaultHostname() string {
	return "expanse-" + i.NodeID.String()[:6]
}
