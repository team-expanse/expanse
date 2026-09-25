# Security: CA rotation and OIDC login

How to rotate the cluster's root CA and how to set up SSO (OIDC) login for the web UI. See
`.plan/ARCHITECTURE.md` §9 (A39–A42) for the design rationale; this document is the operator-facing
companion, in the same spirit as `docs/OBSERVABILITY.md`.

## 1. The design, in one paragraph

Every node holds an Ed25519 cluster CA and a certificate it uses for internal mTLS; both were
independently reviewed for real security defects before this phase extended either (A40 — 4 findings,
all fixed). The CA can be rotated to a fresh root with zero downtime: rotation is treated as "every
node renews its certificate, but onto a new CA" rather than a separate mechanism, reusing the
already-existing renewal loop and the CA bundle's own "trust two CAs during rotation" design (A41).
The web UI's password login is untouched and stays the default; OIDC is a second, purely additive
login path that issues the exact same store-backed session, gated by a required, fail-closed allow-list
of verified email addresses (A39). Neither feature needs a TPM — key material is sealed with age,
keyed by HKDF over the cluster secret; TPM sealing is a named, deliberately deferred future feature
(A42), not implemented here.

## 2. Rotating the cluster CA

Rotation is a three-step CLI flow, run from any enrolled node. Unlike the OIDC commands below, it
reopens the local raft store directly (the same pattern `cluster token create` uses), so **that one
node's own `expanse` daemon must be stopped first** — the rest of the cluster is unaffected and keeps
serving reads and writes throughout.

```sh
systemctl stop expanse
expanse cluster ca rotate
systemctl start expanse
```

This generates a fresh root CA and writes it as the new primary; the old CA stays trusted alongside it
(both are accepted by every TLS listener immediately — no restart needed anywhere else in the
cluster). Every node picks up the new CA and reissues its own certificate the next time its renewal
loop ticks (default every 6h; `--renewal-period` / `expanse.agent.renewalPeriod` to override for
testing). Check progress from any node:

```sh
expanse cluster ca status
```

reports `rotating: primary CA fingerprint <sha256>` plus either the list of nodes still pending or
confirmation that every node has caught up. Once every node has renewed, retire the old CA:

```sh
systemctl stop expanse
expanse cluster ca complete
systemctl start expanse
```

`ca complete` refuses (with a clear error) if any node is still pending, so it is safe to run
speculatively — it only ever succeeds once rotation has actually finished. After it succeeds, the old
CA's key is discarded and it is no longer trusted by anything.

**Operationally:** rotate on a normal maintenance cadence or in response to a real suspected
compromise. There is no fixed validity period forcing rotation — node certificates renew themselves
indefinitely under whichever CA is currently primary.

## 3. Setting up OIDC (SSO) login

OIDC login is off by default; the login page shows only the password form until it is configured.
Configuring it does not disable or replace password login — both remain available side by side.

### 3.1 Register a client at your identity provider

Register Expanse as a confidential OAuth2 client at your IdP (Okta, Google Workspace, Dex, Keycloak,
or any spec-compliant OIDC provider). You will need:

- The **issuer URL** (the base URL your IdP serves `/.well-known/openid-configuration` from).
- A **client ID** and **client secret**.
- The exact **redirect URI** to register: `https://<your-ui-hostname>:8443/login/oidc/callback` — every
  node's certificate carries the shared `expanse-ui` SAN (`.plan/ARCHITECTURE.md` A18), so this is
  normally `https://expanse-ui:8443/login/oidc/callback` regardless of which node currently answers.

### 3.2 Configure Expanse

Run from any node, against its own running daemon (no daemon restart needed — this reads the cluster
secret locally to seal the client secret, but never touches the raft log directly):

```sh
expanse ctl oidc configure \
  --issuer https://idp.example.com \
  --client-id expanse \
  --client-secret '<client secret from your IdP>' \
  --redirect-url https://expanse-ui:8443/login/oidc/callback \
  --allow-email alice@example.com,bob@example.com
```

`--allow-email` is **required and fail-closed by design**: this project has exactly one privilege
level (a single admin account, `.plan/PHASE-02-TASKS.md` D4), and a real corporate IdP may hold
thousands of accounts with no relationship to this cluster — so "any account the IdP will vouch for"
is never the default. Only the listed, comma-separated addresses may log in via SSO; an IdP account
that does not present a `email_verified: true` claim is refused regardless of the allow-list.

Once configured, the login page offers a "Sign in with SSO" link alongside the existing password
form. Re-running `oidc configure` overwrites the previous configuration (e.g. to add or remove an
allowed email, or rotate the client secret) — there is no separate "update" command.

### 3.3 What a login actually does

Signing in via SSO drives a standard authorization-code round trip: Expanse redirects to your IdP
with a fresh CSRF `state` and replay-protection `nonce`; your IdP authenticates the user and redirects
back with a code; Expanse exchanges it, verifies the ID token's signature and `nonce`, and checks the
`email`/`email_verified` claims against the allow-list. On success it issues the identical
store-backed session password login issues — indistinguishable to every other part of the UI, and
honored by whichever node's own listener next answers a request, exactly like a password-issued
session already is.

## 4. What is not covered

TPM-backed sealing of the cluster secret, the CA's private key, or the OIDC client secret — no TPM
hardware has been available to test against, so none of this project's sealing code has ever touched
a real one; it remains age-encrypted, keyed by HKDF over the cluster secret, and is recorded as a
named future feature (`.plan/ARCHITECTURE.md` A42) rather than silently dropped. Identity-provider-side
configuration (registering the client, provisioning users, group/role mapping) is real,
provider-specific work outside this project's scope, same as Alertmanager routing is for
`docs/OBSERVABILITY.md`.
