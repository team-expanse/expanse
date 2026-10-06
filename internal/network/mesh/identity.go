// Package mesh implements the WireGuard overlay (PHASE05.md §4.1):
// per-node identity, and (later cards) the exp0 reconciler.
//
// Identity: each node generates a WireGuard keypair on join. The private
// key lives only on the node (/persist/expanse/network/wg.key, 0600); the
// public key is published to /nodes/<id>/network.wgPublicKey in the
// cluster store so peers can build the full mesh.
package mesh

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/network/addrplan"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/proto"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	pbproto "google.golang.org/protobuf/proto"
)

// Well-known paths and store keys for the mesh.
const (
	// KeyRelPath is the private-key file, relative to the persist dir
	// (/persist/expanse by default in production). Never leaves the node.
	KeyRelPath = "network/wg.key"
)

// Endpoint formats a WireGuard endpoint "host:port".
func Endpoint(host string, port int) string {
	return host + ":" + strconv.Itoa(port)
}

// PublicKeyKey returns the store key for node's published peer record.
func PublicKeyKey(nodeID string) store.Key {
	return store.Key("/nodes/" + nodeID + "/network.wgPublicKey")
}

// Identity is this node's WireGuard identity: the private key (never
// published) and the peer record destined for the store.
type Identity struct {
	Private wgtypes.Key
	Peer    *proto.WireGuardPeer
}

// EnsureIdentity loads (or creates) the node's WireGuard keypair and makes
// sure its public half is published in the store.
//
// Persistence rules (§4.1):
//   - privFile: full path to the private key (typically
//     /persist/expanse/network/wg.key). Created 0600 in a 0700 directory
//     if missing; reused verbatim if present — a restart must never rotate
//     the key, or every peer's AllowedIPs go stale.
//
// Publication rules: the store record is written with CAS expect-missing;
// if a record already exists it must match the local public key exactly —
// a mismatch means the private key was regenerated or the store record is
// corrupt, and proceeding silently would blackhole the node's overlay
// traffic. That case is a hard error, not an overwrite.
func EnsureIdentity(ctx context.Context, st store.Store, nodeID string, privFile string, endpoint string) (*Identity, error) {
	priv, err := loadOrCreateKey(privFile)
	if err != nil {
		return nil, err
	}

	idx, err := ClaimIndex(ctx, st, nodeID)
	if err != nil {
		return nil, err
	}
	prefix, err := addrplan.OverlayPrefix(idx)
	if err != nil {
		return nil, errors.New(errors.KindInvalid, "mesh.identity", err.Error())
	}

	peer := &proto.WireGuardPeer{
		NodeId:        nodeID,
		PublicKey:     priv.PublicKey().String(),
		OverlayPrefix: prefix.String(),
		Endpoint:      endpoint,
	}
	if err := publish(ctx, st, nodeID, peer); err != nil {
		return nil, err
	}
	return &Identity{Private: priv, Peer: peer}, nil
}

// loadOrCreateKey reads the private key from path, generating and persisting
// it (0600) if the file doesn't exist. The file holds the base64 key form.
//
// Concurrency: creation is guarded by O_EXCL. A racing starter either (a)
// loses the EEXIST race and re-reads (never regenerates), or (b) reads the
// file in the window between the winner's create and its write — a zero-
// byte file — and retries briefly. Either way every starter converges on
// the one key that was written first.
func loadOrCreateKey(path string) (wgtypes.Key, error) {
	const retries = 50
	const retryWait = 2 * time.Millisecond // ≤100 ms total
	for attempt := 0; ; attempt++ {
		b, err := os.ReadFile(path)
		if err == nil {
			if len(b) == 0 && attempt < retries {
				// Creator has the file open but hasn't written yet.
				time.Sleep(retryWait)
				continue
			}
			if len(b) == 0 {
				// Persistently empty: the create raced a hard crash
				// (durable entry, lost content). No starter is mid-
				// write (retries exhausted), so the entry is debris —
				// remove it and fall through to regeneration.
				_ = os.Remove(path)
				continue
			}
			k, parseErr := wgtypes.ParseKey(string(b))
			if parseErr != nil {
				return wgtypes.Key{}, errors.New(errors.KindInternal, "mesh.parseKey",
					"corrupt WireGuard key file "+path+": "+parseErr.Error())
			}
			return k, nil
		}
		if !os.IsNotExist(err) {
			return wgtypes.Key{}, errors.New(errors.KindInternal, "mesh.readKey", err.Error())
		}
		if attempt >= retries {
			return wgtypes.Key{}, errors.New(errors.KindInternal, "mesh.readKey",
				"key file "+path+" never became readable after retries")
		}

		k, genErr := wgtypes.GeneratePrivateKey()
		if genErr != nil {
			return wgtypes.Key{}, errors.New(errors.KindInternal, "mesh.generateKey", genErr.Error())
		}
		if mkErr := os.MkdirAll(filepath.Dir(path), 0o700); mkErr != nil {
			return wgtypes.Key{}, errors.New(errors.KindInternal, "mesh.keyDir", mkErr.Error())
		}
		// O_CREATE|O_EXCL: exactly one starter becomes the creator; the
		// rest loop back and read the winner's file.
		f, openErr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if openErr != nil {
			if os.IsExist(openErr) {
				time.Sleep(retryWait)
				continue
			}
			return wgtypes.Key{}, errors.New(errors.KindInternal, "mesh.writeKey", openErr.Error())
		}
		if _, wErr := f.WriteString(k.String()); wErr != nil || f.Sync() != nil || f.Close() != nil {
			_ = f.Close()
			return wgtypes.Key{}, errors.New(errors.KindInternal, "mesh.writeKey", "persisting "+path)
		}
		// Crash-consistency: the key is written ONCE and must survive a
		// hard power loss (qemu quit, kernel panic). The content that
		// was only in the host's volatile cache is gone on such an
		// event — the durable directory entry must not be able to
		// outlive the key bytes (an empty key file wedges the node:
		// the mesh record keeps the OLD key, the file can never be
		// parsed again). Fsync the file, then the directory entry.
		if d, dErr := os.Open(filepath.Dir(path)); dErr == nil {
			_ = d.Sync()
			_ = d.Close()
		}
		return k, nil
	}
}

// publish CAS-writes the peer record. An existing record must carry the
// same identity (node, key, prefix) as the local one — a mismatch means
// the private key was regenerated or the store record is corrupt, and
// proceeding would blackhole the node's overlay traffic (hard error).
// The endpoint, though, may legitimately change (DHCP renewal), so an
// otherwise-identical record is CAS-updated to the current endpoint.
func publish(ctx context.Context, st store.Store, nodeID string, peer *proto.WireGuardPeer) error {
	b, err := pbproto.Marshal(peer)
	if err != nil {
		return errors.New(errors.KindInternal, "mesh.marshalPeer", err.Error())
	}
	k := PublicKeyKey(nodeID)
	if _, casErr := st.CompareAndSwap(ctx, k, 0, b); casErr == nil {
		return nil
	}
	for attempt := 0; attempt < 10; attempt++ {
		cur, getErr := st.Get(ctx, k)
		if getErr != nil {
			return errors.New(errors.KindConflict, "mesh.publish",
				"peer record exists but unreadable: "+getErr.Error())
		}
		var have proto.WireGuardPeer
		if unmarshalErr := pbproto.Unmarshal(cur.Value, &have); unmarshalErr != nil {
			return errors.New(errors.KindConflict, "mesh.publish",
				"stored peer record for "+nodeID+" is corrupt")
		}
		if have.NodeId != peer.NodeId || have.PublicKey != peer.PublicKey ||
			have.OverlayPrefix != peer.OverlayPrefix {
			return errors.New(errors.KindConflict, "mesh.publish",
				"stored public key for "+nodeID+" does not match the local private key — regenerate identity manually")
		}
		if have.Endpoint == peer.Endpoint {
			return nil // fully in sync
		}
		if _, casErr := st.CompareAndSwap(ctx, k, cur.Revision, b); casErr == nil {
			return nil // endpoint refresh
		}
	}
	return errors.New(errors.KindConflict, "mesh.publish",
		"could not refresh endpoint for "+nodeID)
}
