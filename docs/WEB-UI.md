# Web UI

How to reach the web management interface, how its initial credential works, and how to reset it.
See `.plan/ARCHITECTURE.md` §9 (decisions D1–D9) for the design rationale; this document is the
operator-facing companion, mirroring `docs/STORAGE.md`'s role for Phase 1.

## 1. Reaching it

Every node with `expanse.agent.enable = true` serves the UI on `:8443` (`config.PortUI`), over
TLS, as soon as its agent starts — there is no separate service to enable. Point a browser at
`https://<any-node>:8443/` and log in (§2); which node you land on doesn't matter, since blocks,
volumes and cluster state are all cluster-wide views regardless of which node answers.

If the cluster also has the UI's own VIP configured (D8 — a `vip.Holder` wired alongside the
block-VIP path, present whenever `expanse.agent.externalVIPPool`/`externalInterface` are set), one
address is reachable regardless of which node currently holds it, and survives losing that node
(§4). The certificate every node presents carries a fixed shared SAN, `expanse-ui`
(`ca.UIVIPHostname`), in addition to the node's own ID — resolve that name to the VIP address (a
DNS entry, or `/etc/hosts`, or curl's `--resolve`) and the hostname check passes no matter which
node answers.

The certificate is issued by the cluster's own **web UI CA**: ECDSA P-256, created once per cluster
and shared through the store. It is separate from the cluster CA because browsers do not accept
the cluster CA's Ed25519 certificates for TLS (they fail with `SSL_ERROR_NO_CYPHER_OVERLAP`);
node-to-node mTLS still uses the cluster CA. The certificate names `expanse-ui`, the node's ID and
hostname, `localhost`, and the node's addresses, so `https://<node-ip>:8443/` verifies directly.

A browser shows it as untrusted until the UI CA (`/persist/expanse/ca/ui-ca.pem` on any node) is
imported as a trusted authority:

```sh
scp root@<node>:/persist/expanse/ca/ui-ca.pem .
# Firefox: Settings → Privacy & Security → Certificates → View Certificates → Authorities → Import
#          (tick "Trust this CA to identify websites")
# Chrome/OS: import into the system trust store, e.g. on NixOS security.pki.certificateFiles
```

For scripting/`curl`, pass `--cacert /persist/expanse/ca/ui-ca.pem` instead.

## 2. The initial admin credential

There is exactly one account, `admin` (`webauth.AdminUsername`), and it always exists — never a
default or blank password (X3). The first time any node's agent reaches the point of opening its
store, it checks for an admin record and, if none exists yet, generates a random password, hashes
it (argon2id, D3 — no reversible encoding, ever, on disk or in a log) and stores only the hash. The
plaintext password is logged exactly once, at `WARN`, and is never shown again:

```
generated initial web UI admin password — save it now, it will not be shown again  password=<...>
```

This happens uniformly on every node's first `Run()`, not just a fresh install — a restart or a
rejoin never leaves the account unset, and a 3-node cluster only pays this cost once (whichever
node's store write lands first wins; the others see the record already exists). Capture the
password from that node's log right after first boot; if it's missed, reset it (§3) rather than
searching further, since there is no way to recover it after the log line scrolls past.

## 3. Resetting the password

There is deliberately no "forgot password" flow reachable from the browser — the reset path is
`expanse ctl admin reset-password`, run locally against a node's own agent socket, at the same
trust level every other `ctl` command already assumes (anyone with a shell on a cluster node can
already read every secret the agent manages):

```sh
# Generate a new random password (shown once, then never again):
expanse ctl admin reset-password --socket /run/expanse/agent.sock

# Or set an exact password (useful for scripting/automation):
expanse ctl admin reset-password --socket /run/expanse/agent.sock --password '<your-password>'
```

Either form writes the same record shape the agent's own bootstrap write uses, over the existing
generic KV RPC — there is no bespoke reset RPC. Because sessions live in the replicated Raft store,
not process memory (D2), a reset takes effect cluster-wide immediately; any session issued under
the old password keeps working until it expires or the operator logs out (a reset does not itself
revoke existing sessions).

## 4. Session and CSRF

Logging in issues a session cookie (`HttpOnly`, `Secure`, `SameSite`) backed by a store record
(`/ui/sessions/<id>` → user, expiry), not an in-memory map — the reason a session, and the CSRF
token bound to it, both keep working after the UI VIP fails over to a different node (X7). Every
mutating request (POST/PUT/DELETE) must carry the session's CSRF token, either in an
`X-CSRF-Token` header (HTMX's `hx-headers` sends this automatically from the page) or, for a plain
HTML form post, in a `csrf_token` form field (every such form includes it). A raw `curl` POST must
set one of them explicitly, reading the token back from the `expanse_csrf` cookie the login
response sets.

## 5. What's there

Every page renders inside one shared shell: a sidebar (active item highlighted), a header with
the cluster name and a quorum/health indicator, breadcrumbs, the signed-in user menu (sign out),
a light/dark toggle (follows the system by default; the choice is kept in the browser) and a
footer naming the node that served the page and the Expanse version. Status values are shown as
labelled pills (Running, Degraded, Under-replicated, Unreachable…), never raw enum constants.
Actions confirm through a dialog and report their result as a toast. Pages marked *live* update
over Server-Sent Events, never by polling.

- **Dashboard** (`/`, live) — nodes up/total, quorum and leader, blocks by phase, volumes by
  state, firing alerts and the current generation as summary tiles; a "needs attention" list of
  every alert, unreachable node, degraded/failed volume and unhealthy block; recent store events;
  and the node table.
- **Cluster** (`/cluster`, live) — cluster ID, leader, quorum, generation and the Raft member
  table.
- **Nodes** (`/nodes`, live; `/nodes/<id>`) — every member with its role, membership state
  (Online / Unreachable / Failed / Cordoned / Degraded), the health its agent last reported
  through its 10s heartbeat, addresses and join time. The detail page adds hardware inventory
  (system, CPU, memory, disks, NICs, GPUs), health checks, reconciler status and managed
  resources — **for the node serving the page only**: agents do not publish inventory or check
  results cluster-wide, so another node's page says so and links to that node's own UI.
- **Health & alerts** (`/health`, live) — firing critical alerts (the same rules as
  `deploy/prometheus/expanse-alerts.rules.yml`), quorum, node checks, resource health and volume
  health.
- **Blocks** (`/blocks`, `/blocks/new`, `/blocks/<ns>/<name>` live, `/blocks/<ns>/<name>/logs`
  live) — list with phase counts, deploy from a pasted manifest (schema errors shown inline, the
  catalog listed alongside), a detail page with replica placement, pending-placement reasons and
  conditions, scale, delete (confirmed), and a live log tail per replica.
- **Volumes** (`/volumes`, `/volumes/new`, `/volumes/<name>` live) — list with state counts,
  create (field-level validation), a detail page with the replica table (role, health, resync
  progress, out-of-sync bytes, last seen), snapshots, snapshot/resize (grow-only) forms and
  delete (confirmed). Creation is asynchronous — the page reads "Creating…" until the controller
  places it, then flips to the full view without a reload.
- **Generations** (`/generations`, `/generations/<n>`, `/generations/diff?a=<n>&b=<m>`) — the
  desired-state history newest first with the current one marked, a detail page listing the
  snapshot's keys, a diff of any two generations (added / removed / changed keys), and rollback
  behind a confirm dialog (append-only, like `expanse ctl generation rollback`).
- **Events** (`/events`, live) — store writes as they happen, recorded in memory since the
  serving node started, filterable by type (block, volume, node, cluster, resource, lease, auth,
  other). Session keys are redacted; values are never shown.
- **Settings** (`/settings`) — change the admin password (requires the current one; the
  `expanse ctl admin reset-password` path in §3 remains the recovery route), the OIDC
  configuration as stored (issuer, client ID, redirect URL, allow-list — never the secret), and
  the UI CA: where `ui-ca.pem` lives on disk and a download of the certificate
  (`/settings/ui-ca.pem`).

All of the above are thin `html/template` + HTMX layers directly over control-plane operations the
CLI already exercises (`internal/blocks`, `internal/storage`, the in-process `NodeService`) — the
UI does not add a second implementation of anything, only a second way to reach the first one.
Everything is served from the binary (embedded templates, hand-written CSS, vendored htmx); the
page sets a `Content-Security-Policy` that allows no inline script and no third-party origin, so
it works on an offline install.
