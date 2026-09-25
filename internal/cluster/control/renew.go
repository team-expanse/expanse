package control

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/expanse/expanse/internal/cluster/ca"
	"github.com/expanse/expanse/internal/cluster/join"
	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
)

// RenewalInterval is the default period between renewal-loop ticks in
// production: short enough that a 30-day cert's 10-day renewal window
// (ca.NodeCertRenewLeft) is never missed by more than a small margin,
// long enough that the steady-state cost (one store read a tick) is
// negligible. Tests inject a much shorter interval.
const RenewalInterval = 6 * time.Hour

// RunRenewalLoop runs MaybeRenew immediately, then every interval, until
// ctx is done. Every cluster node runs it, not just the leader: any
// node's own cert can need routine renewal, and any node may be
// mid-rotation and not yet reissued onto the new primary — consistent
// with the flat-trust design every node already has (every joiner holds
// the sealed CA key, Phase 10 X1 review), and needed because leadership
// can move to a node that hasn't renewed yet.
func RunRenewalLoop(ctx context.Context, st store.Store, dataDir, nodeID string, secret []byte, interval time.Duration, logger *slog.Logger) {
	if interval <= 0 {
		interval = RenewalInterval
	}
	renewOnce := func() {
		if renewed, err := MaybeRenew(ctx, st, dataDir, nodeID, secret, time.Now()); err != nil {
			if logger != nil {
				logger.Error("cert renewal failed", "err", err)
			}
		} else if renewed && logger != nil {
			logger.Info("node certificate renewed")
		}
	}
	renewOnce()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			renewOnce()
		}
	}
}

// MaybeRenew reissues this node's certificate if it's due: either
// routine expiry (ca.NeedsRenewal, ≤10 days left of the 30-day
// validity) or because a CA rotation (X2) has moved the primary CA and
// this node hasn't reissued under it yet. Rotation is deliberately not
// a separate code path — D3: "renew, but the target CA changed" — so a
// node catches up the moment this loop next ticks, however far into
// rotation it starts. Returns whether it renewed.
func MaybeRenew(ctx context.Context, st store.Store, dataDir, nodeID string, secret []byte, now time.Time) (bool, error) {
	certPEM, err := os.ReadFile(filepath.Join(dataDir, NodeCertFile))
	if err != nil {
		return false, errors.New(errors.KindNotFound, "control.MaybeRenew", "node cert missing: "+err.Error())
	}
	myCert, err := ca.UnmarshalCert(certPEM)
	if err != nil {
		return false, errors.New(errors.KindInternal, "control.MaybeRenew", "corrupt node cert: "+err.Error())
	}

	trust, _, err := LoadCATrust(ctx, st)
	if err != nil {
		return false, errors.Wrap(err, errors.KindUnavailable, "control.MaybeRenew", "load CA trust: "+err.Error())
	}
	primaryCert, err := ca.UnmarshalCert(trust.Primary.CertPEM)
	if err != nil {
		return false, errors.New(errors.KindInternal, "control.MaybeRenew", "corrupt primary CA: "+err.Error())
	}

	// onPrimary checks the signature directly (not a full chain/pool
	// verify) so it answers exactly "did the CURRENT primary issue my
	// cert", regardless of NeedsRenewal's expiry math.
	onPrimary := myCert.CheckSignatureFrom(primaryCert) == nil
	if onPrimary && !ca.NeedsRenewal(myCert, now) {
		return false, nil
	}

	primary, err := trust.Primary.ca(secret)
	if err != nil {
		return false, errors.Wrap(err, errors.KindInternal, "control.MaybeRenew", "load signing CA: "+err.Error())
	}

	cert, priv, err := primary.IssueNode(nodeID, hostnameOrLocal(), nil, now)
	if err != nil {
		return false, errors.Wrap(err, errors.KindInternal, "control.MaybeRenew", "issue cert: "+err.Error())
	}
	keyPEM, err := ca.KeyPEM(priv)
	if err != nil {
		return false, errors.Wrap(err, errors.KindInternal, "control.MaybeRenew", "encode key: "+err.Error())
	}
	if err := saveNodeTLS(dataDir, ca.MarshalCert(cert), keyPEM); err != nil {
		return false, errors.Wrap(err, errors.KindInternal, "control.MaybeRenew", "save cert: "+err.Error())
	}
	// Keep the disk CA cache in step with whichever CA actually signed:
	// matters most right after a rotation, so a restart before this
	// node's own reissue still boots (LoadCluster) with a CA whose key
	// it can use to serve joins if it becomes leader.
	if err := saveCA(dataDir, primary, secret); err != nil {
		return false, errors.Wrap(err, errors.KindInternal, "control.MaybeRenew", "update CA cache: "+err.Error())
	}

	if err := stampCAFingerprint(ctx, st, nodeID, ca.Fingerprint(primary.Cert)); err != nil {
		return false, errors.Wrap(err, errors.KindInternal, "control.MaybeRenew", "record renewal: "+err.Error())
	}
	return true, nil
}

// stampCAFingerprint records this node's currently-active CA fingerprint
// on its own /nodes/<id> record — CARotationStatus reads it across every
// node to know when the outgoing CA can be safely retired. Bounded
// retry against concurrent writes to the same record (mirrors
// nodelc.Remove's cleanup retry).
func stampCAFingerprint(ctx context.Context, st store.Store, nodeID, fp string) error {
	key := store.Key(join.NodesKeyPrefix + nodeID)
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		cur, err := st.Get(ctx, key)
		if err != nil {
			return err
		}
		var rec join.NodeRecord
		if err := json.Unmarshal(cur.Value, &rec); err != nil {
			return errors.New(errors.KindInternal, "control.stampCAFingerprint", "corrupt node record: "+err.Error())
		}
		if rec.CAFingerprint == fp {
			return nil // already stamped (e.g. a retried tick)
		}
		rec.CAFingerprint = fp
		v, err := json.Marshal(rec)
		if err != nil {
			return errors.New(errors.KindInternal, "control.stampCAFingerprint", "encode: "+err.Error())
		}
		if _, err := st.CompareAndSwap(ctx, key, cur.Revision, v); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return errors.Wrap(lastErr, errors.KindConflict, "control.stampCAFingerprint", "too many concurrent updates")
}
