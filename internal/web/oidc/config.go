// Package oidc implements OIDC login for the web UI (ROADMAP.md Phase
// 10, X3): an operator-configured external identity provider as a
// second login path alongside internal/web/auth's password login,
// issuing the exact same store-backed auth.Session every other login
// path already uses (EXTERNAL-COMPONENTS.md D2's exit condition -- OIDC
// is additive, never a replacement). Built on coreos/go-oidc/v3 +
// golang.org/x/oauth2 (D2's adoption pick): this package is deliberately
// thin glue -- discovery, code exchange, ID-token verification and
// claim mapping -- not a second hand-rolled JWT/JWKS implementation.
package oidc

import (
	"context"
	"encoding/json"
	"time"

	"github.com/expanse/expanse/internal/cluster/ca"
	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
)

// ConfigKey is the store key holding the OIDC login configuration --
// store-backed rather than a new config surface, the same pattern
// internal/metrics' scrape token and internal/web/auth's admin record
// already use (tokens.go's seam, PHASE-10-TASKS.md §1).
const ConfigKey = "/ui/oidc/config"

// Config is the operator-supplied OIDC relying-party configuration.
type Config struct {
	Issuer   string `json:"issuer"`
	ClientID string `json:"client_id"`
	// SealedSecret is the client secret, age-encrypted to the cluster
	// secret (ca.SealBytes) exactly as the CA private key is -- it must
	// be recoverable to present at the token endpoint, so (unlike the
	// admin password and metrics token) it cannot just be hashed.
	SealedSecret []byte `json:"sealed_secret"`
	// RedirectURL is the exact, pre-registered callback URL (OIDC
	// requires an exact match at the IdP) -- an operator-chosen value,
	// not derived from the inbound request's Host header, since this
	// cluster may be reached at more than one name (per-node hostname
	// or the UI VIP, ca.UIVIPHostname).
	RedirectURL string `json:"redirect_url"`
	// AllowedEmails is a required, non-empty allow-list of verified
	// email claims authorized to log in. A real corporate IdP (Okta,
	// Google Workspace) may hold thousands of accounts unrelated to
	// this cluster, and this project has exactly one privilege level
	// (ROADMAP.md Phase 2 D4) -- so "any account the IdP will vouch
	// for" is never the default. Fail closed, matching the discipline
	// Phase 10 Stream A's security review already established for this
	// codebase's other auth checks.
	AllowedEmails []string `json:"allowed_emails"`
	CreatedAt     int64    `json:"created"` // unix-nano
}

// NewConfigRecord validates and JSON-encodes a Config for ConfigKey.
// Exported so `expanse ctl oidc configure` can write it directly over
// the existing generic KV RPC, mirroring admin reset-password / metrics
// set-token -- no bespoke RPC for this either (Stream B's same scope
// discipline). clientSecret is sealed with clusterSecret before
// encoding; the caller must have read it locally (control.LoadCluster),
// since the CLI does not hold it over the agent socket.
func NewConfigRecord(issuer, clientID, clientSecret, redirectURL string, allowedEmails []string, clusterSecret []byte) ([]byte, error) {
	if issuer == "" || clientID == "" || clientSecret == "" || redirectURL == "" {
		return nil, errors.New(errors.KindInvalid, "oidc.NewConfigRecord", "issuer, client-id, client-secret and redirect-url are all required")
	}
	if len(allowedEmails) == 0 {
		return nil, errors.New(errors.KindInvalid, "oidc.NewConfigRecord", "at least one --allow-email is required (fail closed: no default allow-all)")
	}
	sealed, err := ca.SealBytes([]byte(clientSecret), clusterSecret)
	if err != nil {
		return nil, errors.Wrap(err, errors.KindInternal, "oidc.NewConfigRecord", "seal client secret: "+err.Error())
	}
	rec := Config{
		Issuer: issuer, ClientID: clientID, SealedSecret: sealed,
		RedirectURL: redirectURL, AllowedEmails: allowedEmails,
		CreatedAt: time.Now().UnixNano(),
	}
	v, err := json.Marshal(rec)
	if err != nil {
		return nil, errors.New(errors.KindInternal, "oidc.NewConfigRecord", err.Error())
	}
	return v, nil
}

// LoadConfig reads ConfigKey. errors.KindNotFound means OIDC login is
// simply not configured -- the caller's job to treat as "feature
// disabled", never as a startup failure (D2's additive-only exit).
func LoadConfig(ctx context.Context, st store.Store) (*Config, error) {
	e, err := st.Get(ctx, store.Key(ConfigKey))
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(e.Value, &cfg); err != nil {
		return nil, errors.New(errors.KindInternal, "oidc.LoadConfig", "corrupt OIDC config record: "+err.Error())
	}
	return &cfg, nil
}

// IsConfigured is a cheap existence check for rendering the login
// page's optional SSO button. Best-effort: any error (including a
// transient store one) means "don't show the button" -- the real
// authorization gate is HandleCallback, not this.
func IsConfigured(ctx context.Context, st store.Store) bool {
	_, err := LoadConfig(ctx, st)
	return err == nil
}

// clientSecret unseals cfg.SealedSecret with clusterSecret.
func (cfg *Config) clientSecret(clusterSecret []byte) (string, error) {
	b, err := ca.UnsealBytes(cfg.SealedSecret, clusterSecret)
	if err != nil {
		return "", errors.Wrap(err, errors.KindInternal, "oidc.clientSecret", "unseal: "+err.Error())
	}
	return string(b), nil
}
