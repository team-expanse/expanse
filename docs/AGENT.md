# The Node Agent (`expansed`)

The agent turns desired state into actual system state, continuously and
idempotently. This document explains the model with a worked example.

## Architecture

```
  gRPC unix socket ──► api.Server (NodeService)
                          │
                          ▼
                     Store (interface)      ◄── BoltDB at
                          │ watch               /persist/expanse/store/local.db
                          ▼
                     Reconciler (tick loop)
                          │
                          ▼
              Resource Managers (file, directory, sysctl,
                                 systemd-unit, nix-config)
```

## The store

`internal/store` defines the interface; `internal/store/boltstore` is the
Phase 02 implementation (Phase 03 swaps in a Raft-backed store behind the
same interface — both run the same conformance suite in
`internal/store/conformance`).

Key facts:

- **Revisions** are strictly monotonic across all keys; every write
  returns one.
- **CAS**: `CompareAndSwap(k, expect, v)` — `expect == 0` means "must not
  exist"; a stale revision returns a `conflict` error and changes nothing.
- **Prefixes are literal bytes**: `List("/a")` returns `/ab` too. Always
  use a trailing slash for directory semantics.
- **Watch** delivers events under a prefix in revision order, never
  silently dropping: if a watcher's 1024-event buffer overflows, the
  channel is *closed* and the caller must re-list and re-watch.

## The reconcile loop

One tick (`internal/reconcile`):

1. Read desired state under `/node/<self>/resources/`.
2. Build the resource DAG, topologically sort it; a cycle aborts the tick
   with an error status.
3. For each resource in dependency order (independent resources in
   parallel, bounded by `GOMAXPROCS`, max 8):
   - **Observe** (30 s timeout) — read actual state.
   - **Plan** — compare desired vs observed; produce ordered actions.
   - **Apply** each action (120 s timeout), then re-observe to verify.
   - Record status at `/node/<self>/status/resources/<id>`.
4. Write aggregate node status and metrics.

Timing:

- Base tick: 30 s (level-triggered safety net).
- Watch-triggered: any change under the resources prefix triggers a tick,
  debounced 500 ms. Ticks are idempotent, so extra ticks are cheap.
- Per-tick deadline: 5 min.
- **Failure backoff** per resource: 1, 2, 4, 8, 16, 30, 30 s… with ±20 %
  jitter (no thundering herd). A success resets the backoff. A resource in
  backoff is *deferred*, not failed.

A tick is **idempotent**: running it twice on an in-sync system applies
zero changes. This is asserted by tests (`agent-reconcile` VM test,
reconciler unit tests).

## Resource managers

Each manager handles one resource type: `Observe` reads ground truth,
`Plan` returns actions, `Apply` executes them idempotently.

| Type | Spec | Notes |
|---|---|---|
| `file` | path, content, mode, owner | atomic write: temp file → fsync → rename |
| `directory` | path, mode | `MkdirAll` + chmod; deletes only if empty |
| `sysctl` | key, value | writes `/proc/sys/...`; runtime-only (reboots revert — use `nix-config` for persistence) |
| `systemd-unit` | name, enabled, state | systemd D-Bus API, never `systemctl` output parsing |
| `nix-config` | flake, attr, switch_mode | build via the nix driver, then `switch-to-configuration` |

Deleting a resource (`expanse ctl resource delete <id>`) removes the
desired state **and**, for types that support it (`file`, `directory`,
`systemd-unit`), the managed object itself.

## Worked example: a motd file

Apply a spec document:

```console
$ expanse ctl resource apply - <<'EOF'
file:/etc/motd:
  type: file
  path: /etc/motd
  content: "welcome to expanse"
  mode: "0644"
EOF
applied: 1  failed: 0
```

The agent writes `/node/<self>/resources/file:/etc/motd` in the store; the
watch fires; a debounced tick runs ~500 ms later:

1. The `file` manager observes `/etc/motd`: missing → `Exists=false`,
   `InSync=false`.
2. Plan: one action — *write /etc/motd (19 bytes, mode 0644)*.
3. Apply writes `/etc/motd.expanse-tmp`, fsyncs, renames, chmods. Readers
   never see a torn file.
4. Re-observe: in sync. Status recorded, `changes_applied` → 1.

Now drift:

```console
$ echo hacked > /etc/motd
# …within ≤ 30 s (the level-triggered tick):
$ cat /etc/motd
welcome to expanse
```

Reconcile again — nothing to do:

```console
$ expanse ctl reconcile
[starting] reconcile requested
[complete] (100%) 1 resources, 0 changes, 0 failures (took 1 ms)
```

Delete:

```console
$ expanse ctl resource delete file:/etc/motd
deleted: true
$ ls /etc/motd
ls: cannot access '/etc/motd': No such file or directory
```

## Dependencies

Resources may declare dependencies on other resource IDs; dependents run
strictly after their dependencies converge. Cycles abort the tick (the
desired state is broken and must be fixed, not ignored).

## Nix switches and the watchdog

`nix-config` resources build a flake attribute and run
`switch-to-configuration`. Because a remote config change must never be
able to brick a node:

1. Before every switch the driver writes
   `/persist/expanse/pending-switch` containing the previous system path.
2. On success the marker is cleared.
3. `expanse-switch-watchdog.timer` runs `expanse watchdog` every minute:
   if the marker is stale (> 10 min — the agent died mid-switch), it rolls
   back to the previous generation and reboots.

## systemd integration

`expansed.service` is `Type=notify`: the agent signals `READY=1` after the
store opens, the socket binds, and the first reconcile completes, and pings
`WATCHDOG=1` every 30 s (`WatchdogSec=60s`). A wedged agent restarts
automatically.

## API surface

gRPC on `unix:///run/expanse/agent.sock` (mode 0660, group `expanse` —
filesystem permissions are the auth). TCP :7443 exists as a flag but is
**off by default** in Phase 02; Phase 03 enables it with mTLS.

`expanse ctl` covers every RPC: `node status|inspect|health|shutdown`,
`resource list|get|apply|delete`, `reconcile [--dry-run]`, `events
[--follow]`, `version`. `--output json` is stable and scriptable; table
output is for humans and may change.
