package control

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/expanse/expanse/internal/cluster/ca"
	"github.com/expanse/expanse/internal/cluster/join"
	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
)

// CATrustKey is the Raft-replicated source of truth for which CA(s) the
// cluster currently trusts. Every node's own disk cache (paths.go's
// CAFile/CAKeyFile) is a bootstrap convenience only — this record is
// authoritative, since rotation (X2) needs every node to agree on the
// same primary/outgoing pair, not whichever CA a node's disk happens to
// still hold.
const CATrustKey = "/cluster/ca/trust"

// CAEntry is one CA's material as stored in CATrust: the cert plus its
// private key, age-sealed to the cluster secret exactly as paths.go's
// on-disk CAKeyFile is — the store already carries equally sensitive
// material (the sealed CA key is handed to every joiner in-band, D-note
// in join.Service.sealedCAKey), so this is not a new exposure.
type CAEntry struct {
	CertPEM   []byte `json:"cert"`
	SealedKey []byte `json:"sealed_key"`
}

// CATrust is the CATrustKey record. Outgoing is nil except during a CA
// rotation (RotateCA sets it, CompleteCARotation clears it), mirroring
// ca.Bundle's "normally one CA, two during rotation" design.
type CATrust struct {
	Primary  CAEntry  `json:"primary"`
	Outgoing *CAEntry `json:"outgoing,omitempty"`
}

func entryFor(c *ca.CA, secret []byte) (CAEntry, error) {
	sealed, err := ca.SealKey(c.Priv, secret)
	if err != nil {
		return CAEntry{}, err
	}
	return CAEntry{CertPEM: ca.MarshalCert(c.Cert), SealedKey: sealed}, nil
}

func (e CAEntry) ca(secret []byte) (*ca.CA, error) {
	cert, err := ca.UnmarshalCert(e.CertPEM)
	if err != nil {
		return nil, errors.New(errors.KindInternal, "control.CATrust", "corrupt CA cert: "+err.Error())
	}
	priv, err := ca.UnsealKey(e.SealedKey, secret)
	if err != nil {
		return nil, errors.New(errors.KindInternal, "control.CATrust", "unseal CA key: "+err.Error())
	}
	return &ca.CA{Priv: priv, Cert: cert}, nil
}

// InitCATrust writes the cluster's first CATrust record: just the
// bootstrap CA, no rotation in progress. Called once by Init; CAS
// expect-absent so a re-run (or a joiner racing the leader's own write)
// never clobbers it.
func InitCATrust(ctx context.Context, st store.Store, secret []byte, clusterCA *ca.CA) error {
	entry, err := entryFor(clusterCA, secret)
	if err != nil {
		return errors.Wrap(err, errors.KindInternal, "control.InitCATrust", "seal CA: "+err.Error())
	}
	v, err := json.Marshal(CATrust{Primary: entry})
	if err != nil {
		return errors.New(errors.KindInternal, "control.InitCATrust", "encode: "+err.Error())
	}
	if _, err := st.CompareAndSwap(ctx, store.Key(CATrustKey), 0, v); err != nil {
		if errors.Is(err, errors.KindConflict) {
			return nil // already initialized (idempotent bootstrap)
		}
		return errors.Wrap(err, errors.KindInternal, "control.InitCATrust", "write: "+err.Error())
	}
	return nil
}

// LoadCATrust reads the current CATrust record.
func LoadCATrust(ctx context.Context, st store.Store) (*CATrust, store.Revision, error) {
	e, err := st.Get(ctx, store.Key(CATrustKey))
	if err != nil {
		return nil, 0, err // callers distinguish KindNotFound (pre-X2 cluster) themselves
	}
	var t CATrust
	if err := json.Unmarshal(e.Value, &t); err != nil {
		return nil, 0, errors.New(errors.KindInternal, "control.LoadCATrust", "corrupt record: "+err.Error())
	}
	return &t, e.Revision, nil
}

// LoadCATrustBundle builds the live ca.Bundle (primary, plus outgoing
// when rotating) from the store. Falls back to a single-CA bundle built
// from fallback when the store has no CATrust record yet — a cluster
// that formed before X2, or a very early boot racing initial
// replication — rather than breaking every TLS listener on it.
func LoadCATrustBundle(ctx context.Context, st store.Store, fallback *ca.CA) (*ca.Bundle, error) {
	trust, _, err := LoadCATrust(ctx, st)
	if err != nil {
		if errors.Is(err, errors.KindNotFound) && fallback != nil {
			return ca.NewBundle(fallback.Cert)
		}
		return nil, err
	}
	primary, err := ca.UnmarshalCert(trust.Primary.CertPEM)
	if err != nil {
		return nil, errors.New(errors.KindInternal, "control.LoadCATrustBundle", "corrupt primary CA: "+err.Error())
	}
	if trust.Outgoing != nil {
		outgoing, err := ca.UnmarshalCert(trust.Outgoing.CertPEM)
		if err != nil {
			return nil, errors.New(errors.KindInternal, "control.LoadCATrustBundle", "corrupt outgoing CA: "+err.Error())
		}
		return ca.NewBundle(primary, outgoing)
	}
	return ca.NewBundle(primary)
}

// RotateCA starts a CA rotation: generates a fresh CA, makes it primary
// (new joins and renewals sign under it), and keeps the current primary
// as Outgoing so certs already issued under it keep verifying until
// CompleteCARotation retires it (D3: "renew, but the target CA changed",
// not a separate mechanism — the existing renewal loop does the rest).
// Refuses (KindConflict) if a rotation is already in progress.
func RotateCA(ctx context.Context, st store.Store, secret []byte, now time.Time) (*ca.CA, error) {
	trust, rev, err := LoadCATrust(ctx, st)
	if err != nil {
		return nil, errors.Wrap(err, errors.KindUnavailable, "control.RotateCA", "load trust: "+err.Error())
	}
	if trust.Outgoing != nil {
		return nil, errors.New(errors.KindConflict, "control.RotateCA", "a rotation is already in progress; run `cluster ca complete` first")
	}
	clusterID := ""
	if oldPrimary, err := ca.UnmarshalCert(trust.Primary.CertPEM); err == nil {
		clusterID = oldPrimary.Subject.CommonName
	}
	fresh, err := ca.Generate(clusterID, now)
	if err != nil {
		return nil, errors.Wrap(err, errors.KindInternal, "control.RotateCA", "generate CA: "+err.Error())
	}
	newEntry, err := entryFor(fresh, secret)
	if err != nil {
		return nil, errors.Wrap(err, errors.KindInternal, "control.RotateCA", "seal new CA: "+err.Error())
	}
	outgoing := trust.Primary
	v, err := json.Marshal(CATrust{Primary: newEntry, Outgoing: &outgoing})
	if err != nil {
		return nil, errors.New(errors.KindInternal, "control.RotateCA", "encode: "+err.Error())
	}
	if _, err := st.CompareAndSwap(ctx, store.Key(CATrustKey), rev, v); err != nil {
		return nil, errors.Wrap(err, errors.KindConflict, "control.RotateCA", "concurrent trust update, retry: "+err.Error())
	}
	return fresh, nil
}

// RotationStatus reports CA rotation progress.
type RotationStatus struct {
	Rotating    bool     // an Outgoing CA is still present
	Fingerprint string   // current primary CA's fingerprint
	Pending     []string // node IDs whose last-stamped CAFingerprint isn't the primary yet
}

// CARotationStatus lists every node record and compares its stamped
// CAFingerprint (control's renewal loop stamps it after every reissue)
// against the current primary, so an operator — or CompleteCARotation —
// can tell whether it's safe to retire the outgoing CA yet.
func CARotationStatus(ctx context.Context, st store.Store) (*RotationStatus, error) {
	trust, _, err := LoadCATrust(ctx, st)
	if err != nil {
		return nil, errors.Wrap(err, errors.KindUnavailable, "control.CARotationStatus", "load trust: "+err.Error())
	}
	primary, err := ca.UnmarshalCert(trust.Primary.CertPEM)
	if err != nil {
		return nil, errors.New(errors.KindInternal, "control.CARotationStatus", "corrupt primary CA: "+err.Error())
	}
	fp := ca.Fingerprint(primary)
	status := &RotationStatus{Rotating: trust.Outgoing != nil, Fingerprint: fp}
	if !status.Rotating {
		return status, nil
	}
	entries, err := st.List(ctx, store.Key(join.NodesKeyPrefix))
	if err != nil {
		return nil, errors.Wrap(err, errors.KindUnavailable, "control.CARotationStatus", "list nodes: "+err.Error())
	}
	for _, e := range entries {
		var rec join.NodeRecord
		if err := json.Unmarshal(e.Value, &rec); err != nil {
			continue
		}
		if rec.CAFingerprint != fp {
			status.Pending = append(status.Pending, rec.ID)
		}
	}
	return status, nil
}

// CompleteCARotation retires the outgoing CA: it stops being trusted at
// all, and its sealed key is dropped from the record. Refuses
// (KindConflict) while any node hasn't renewed onto the primary yet —
// retiring early would make that node's still-in-use cert stop
// verifying anywhere, a self-inflicted outage X2 exists to prevent.
func CompleteCARotation(ctx context.Context, st store.Store) error {
	trust, rev, err := LoadCATrust(ctx, st)
	if err != nil {
		return errors.Wrap(err, errors.KindUnavailable, "control.CompleteCARotation", "load trust: "+err.Error())
	}
	if trust.Outgoing == nil {
		return errors.New(errors.KindConflict, "control.CompleteCARotation", "no rotation in progress")
	}
	status, err := CARotationStatus(ctx, st)
	if err != nil {
		return err
	}
	if len(status.Pending) > 0 {
		return errors.New(errors.KindConflict, "control.CompleteCARotation",
			"nodes not yet renewed onto the new CA: "+strings.Join(status.Pending, ", "))
	}
	v, err := json.Marshal(CATrust{Primary: trust.Primary})
	if err != nil {
		return errors.New(errors.KindInternal, "control.CompleteCARotation", "encode: "+err.Error())
	}
	if _, err := st.CompareAndSwap(ctx, store.Key(CATrustKey), rev, v); err != nil {
		return errors.Wrap(err, errors.KindConflict, "control.CompleteCARotation", "concurrent trust update, retry: "+err.Error())
	}
	return nil
}
