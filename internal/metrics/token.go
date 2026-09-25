// Package metrics implements Phase 9's Prometheus-compatible /metrics
// endpoint: a bearer-token-gated exporter translating this project's
// existing health signals (reconcile.Health, control.Status,
// agent/health) into real Prometheus gauges, and nothing else -- no
// metrics storage, no alert evaluation (both adopted externally, D1/D4).
package metrics

import (
	"context"
	"encoding/json"
	"time"

	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
	webauth "github.com/expanse/expanse/internal/web/auth"
)

// TokenKey is the store key holding the metrics scrape bearer token's
// hash (D2). One token per cluster, not per node -- every node answers
// scrapes, and a token issued by whichever node creates it must
// authorize a scrape any node answers, the same store-backed sharing
// EnsureAdmin's account already relies on.
const TokenKey = "/metrics/token"

type tokenRecord struct {
	Hash      string `json:"hash"`
	CreatedAt int64  `json:"created"` // unix-nano
}

// NewTokenRecord hashes token and returns the JSON-encoded store record
// for TokenKey, using the same argon2id hashing EnsureAdmin's account
// uses -- one hashing implementation, not a second one for this phase.
// Exported so `expanse ctl metrics set-token` can write it directly over
// the existing generic KV RPC, mirroring `admin reset-password` (D2).
func NewTokenRecord(token string) ([]byte, error) {
	hash, err := webauth.HashPassword(token)
	if err != nil {
		return nil, err
	}
	rec := tokenRecord{Hash: hash, CreatedAt: time.Now().UnixNano()}
	v, err := json.Marshal(rec)
	if err != nil {
		return nil, errors.New(errors.KindInternal, "metrics.NewTokenRecord", err.Error())
	}
	return v, nil
}

// EnsureToken creates the scrape token if it does not already exist,
// with a freshly generated random value, and returns it so the caller
// can surface it to the operator exactly once -- the same bootstrap-once
// pattern EnsureAdmin uses for the UI's initial password. Safe to call
// from every node at startup: the CAS expect-absent write means only the
// node that actually creates the record gets a non-empty token back.
func EnsureToken(ctx context.Context, st store.Store) (token string, err error) {
	tok, err := webauth.GenerateResetPassword()
	if err != nil {
		return "", err
	}
	v, err := NewTokenRecord(tok)
	if err != nil {
		return "", err
	}
	if _, err := st.CompareAndSwap(ctx, store.Key(TokenKey), 0, v); err != nil {
		if errors.Is(err, errors.KindConflict) {
			return "", nil // another node already created it; nothing to show
		}
		return "", errors.Wrap(err, errors.KindInternal, "metrics.EnsureToken", "store write failed")
	}
	return tok, nil
}

// VerifyToken reports whether token authorizes a scrape. A missing
// record is treated as "wrong token", never as "no auth required".
func VerifyToken(ctx context.Context, st store.Store, token string) bool {
	e, err := st.Get(ctx, store.Key(TokenKey))
	if err != nil {
		return false
	}
	var rec tokenRecord
	if err := json.Unmarshal(e.Value, &rec); err != nil {
		return false
	}
	return webauth.VerifyPassword(rec.Hash, token)
}
