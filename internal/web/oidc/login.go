package oidc

import (
	"context"
	"slices"

	goidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/web/auth"
)

// scopes requested of every OIDC provider: openid (required) plus email
// (the only claim this package's authorization decision uses) and
// profile (conventional, harmless to request even though unused).
var scopes = []string{goidc.ScopeOpenID, "email", "profile"}

// newFlow builds this login attempt's oauth2.Config and ID-token
// verifier from cfg, doing a live discovery round trip to cfg.Issuer.
// Not cached: OIDC logins are rare, not a hot path (the same trade-off
// internal/web/auth's argon2id parameters already make for password
// login), so a config change takes effect on the very next login
// instead of needing a cache-invalidation path.
func newFlow(ctx context.Context, cfg *Config, clusterSecret []byte) (*oauth2.Config, *goidc.IDTokenVerifier, error) {
	secret, err := cfg.clientSecret(clusterSecret)
	if err != nil {
		return nil, nil, err
	}
	provider, err := goidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, nil, errors.Wrap(err, errors.KindUnavailable, "oidc.newFlow", "discovery against "+cfg.Issuer+": "+err.Error())
	}
	oa := &oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: secret,
		RedirectURL:  cfg.RedirectURL,
		Endpoint:     provider.Endpoint(),
		Scopes:       scopes,
	}
	verifier := provider.Verifier(&goidc.Config{ClientID: cfg.ClientID})
	return oa, verifier, nil
}

// StartLogin begins an authorization-code flow: it returns the IdP's
// authorization URL to redirect the browser to, plus the state and
// nonce the caller must hold (short-lived cookies) and pass back into
// HandleCallback -- CSRF protection (state) and replay protection
// (nonce) for the returned ID token.
func StartLogin(ctx context.Context, cfg *Config, clusterSecret []byte) (authURL, state, nonce string, err error) {
	oa, _, err := newFlow(ctx, cfg, clusterSecret)
	if err != nil {
		return "", "", "", err
	}
	state, err = auth.RandomToken(16)
	if err != nil {
		return "", "", "", err
	}
	nonce, err = auth.RandomToken(16)
	if err != nil {
		return "", "", "", err
	}
	authURL = oa.AuthCodeURL(state, goidc.Nonce(nonce))
	return authURL, state, nonce, nil
}

// HandleCallback exchanges code for tokens, verifies the returned ID
// token's signature, issuer, audience and nonce, and checks its email
// claim against cfg.AllowedEmails. Returns the authorized session
// UserID (the matched email) on success.
func HandleCallback(ctx context.Context, cfg *Config, clusterSecret []byte, code, wantNonce string) (userID string, err error) {
	oa, verifier, err := newFlow(ctx, cfg, clusterSecret)
	if err != nil {
		return "", err
	}
	tok, err := oa.Exchange(ctx, code)
	if err != nil {
		return "", errors.Wrap(err, errors.KindPermission, "oidc.HandleCallback", "code exchange failed: "+err.Error())
	}
	rawIDToken, ok := tok.Extra("id_token").(string)
	if !ok {
		return "", errors.New(errors.KindPermission, "oidc.HandleCallback", "token response had no id_token")
	}
	idTok, err := verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return "", errors.Wrap(err, errors.KindPermission, "oidc.HandleCallback", "id_token verification failed: "+err.Error())
	}
	if idTok.Nonce != wantNonce {
		return "", errors.New(errors.KindPermission, "oidc.HandleCallback", "id_token nonce mismatch")
	}
	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	if err := idTok.Claims(&claims); err != nil {
		return "", errors.Wrap(err, errors.KindInternal, "oidc.HandleCallback", "decode claims: "+err.Error())
	}
	// Required, not just preferred: an unverified email claim is the
	// IdP itself saying it cannot vouch for this address, and this
	// package's whole authorization decision is an email allow-list.
	if claims.Email == "" || !claims.EmailVerified {
		return "", errors.New(errors.KindPermission, "oidc.HandleCallback", "id_token has no verified email claim")
	}
	if !slices.Contains(cfg.AllowedEmails, claims.Email) {
		return "", errors.New(errors.KindPermission, "oidc.HandleCallback", "email not in the configured allow-list")
	}
	return claims.Email, nil
}
