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

The certificate itself is issued by the cluster's own CA (D5 — no operator action needed at
install), not a public one, so a browser will show it as untrusted until the CA certificate
(`/persist/expanse/ca/ca.pem` on any node) is imported into the browser's or OS's trust store. For
scripting/`curl`, pass `--cacert /persist/expanse/ca/ca.pem` instead.

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
mutating request (HTMX's POST/PUT/DELETE) must carry the session's CSRF token in an
`X-CSRF-Token` header (HTMX's `hx-headers` sends this automatically from the page; a raw `curl`
POST must set it explicitly, reading it back from the `expanse_csrf` cookie the login response
sets).

## 5. What's there

- **Cluster overview** (`/cluster`) — nodes, health, quorum, generations, and the event feed, live
  over Server-Sent Events, not polled.
- **Blocks** (`/blocks`) — list, deploy (a pasted or uploaded manifest), scale, delete, and tail
  live logs, live placement/phase over SSE.
- **Volumes** (`/volumes`) — create, resize (grow-only), snapshot, delete, and the replica table
  (state, role, per-node placement), live over SSE. Volume creation is asynchronous — the detail
  page reads "Creating…" until the controller places it, then flips to the full view without a
  page reload.

All of the above are thin `html/template` + HTMX layers directly over control-plane operations the
CLI already exercises (`internal/blocks`, `internal/storage`) — the UI does not add a second
implementation of anything, only a second way to reach the first one.
