# Blocks

Blocks are Expanse's unit of deployment: a declarative description of a
service that the cluster schedules onto nodes and runs as systemd units.
This document walks one block from authoring to a rolling update — the
complete lifecycle, with every command you need. For the clustering
machinery underneath (Raft, leases, node lifecycle), see
[CLUSTERING.md](CLUSTERING.md).

Concepts in one paragraph:

- A **block** is authored as YAML (or Nix), validated against 24 rule
  classes, and stored as a protobuf `Block` in the Raft-replicated KV
  under `/blocks/<namespace>/<name>`.
- The leader-side **controller** places replicas through the
  **scheduler** (filters → anti-affinity/overcommit checks → scoring),
  persists placements in the block status, and the per-node **agent**
  materializes each placement as a `expanse-block@.service` systemd
  unit (config generated from the block's catalog schema + defaults).
- Agents report replica health back through the store; the controller
  promotes placements `SCHEDULING → RUNNING` and the block itself when
  every replica is there. Node death beyond the unreachable grace
  retires the placement and reschedules it. `singleton` blocks are
  fenced by a Raft-backed lease — exactly one instance, ever.

## The worked example: a three-replica web service

### 1. Author `web.yaml`

```yaml
apiVersion: expanse.io/v1
kind: Block
metadata:
  name: web
  namespace: default
  labels: { app: web, tier: front }
spec:
  type: web/nginx                 # catalog type: <category>/<name>
  version: "1.27"

  replicas: 3
  strategy:
    kind: active-active
    update:
      mode: rolling
      maxUnavailable: 1
      maxSurge: 0
      minReadySeconds: 10

  runtime: systemd

  resources:
    requests: { cpu: "500m", memory: "256Mi" }

  placement:
    antiAffinity: node            # never two replicas on one node
    spread: even

  network:
    ports:
      - name: http
        port: 80
        targetPort: 8080
        protocol: tcp
        expose: vip
    healthCheck:
      readiness:
        type: http
        path: /
        port: 8080
        initialDelaySeconds: 5
        periodSeconds: 10
        failureThreshold: 3
```

The readiness probe runs on the node hosting each replica, against the
node's own address and the probe's `port`. Each result is written to
`/blocks/<ns>/<name>/status/replicas/<i>`, and the VIP load balancer and
DNS skip a replica whose probe fails. A new replica is not `RUNNING`
until its probe passes on its own node. `tcp` and `http` probes run;
`exec` probes are not run yet. A VM's readiness comes from its guest
instead (see `docs/VMS.md`). A port exposed on a VIP requires a
readiness probe.

A liveness probe runs the same way. When it fails `failureThreshold`
times in a row, the node restarts the replica's unit in place, then
waits a backoff (10 s, doubling up to 5 min) plus `initialDelaySeconds`
before probing again. Five restarts that do not keep the probe passing
for 10 minutes and the node gives up and marks the replica failed.
Restarts and the failed mark are recorded at
`/blocks/<ns>/<name>/status/liveness/<i>`. The number of restarts is the
agent's `livenessMaxRestarts` option. Liveness probes of VMs are not run
yet.

The controller then marks the placement `FAILED`, stops it and schedules
the replica on another node, never the one it failed on. A singleton
with storage only moves to a node holding a replica of its disk. If the
replica fails on that second node too, it is stopped instead of moved
again, so a broken workload does not cycle through the cluster: the
block goes `FAILED` (or `DEGRADED` while its other replicas run) with
reason `LivenessFailed`. Applying a new version of the block clears
this and places the replica again. A daemonset replica cannot move, so
it is marked `FAILED` and left running on its node. A missing or
unreadable liveness record, or one written by another node, never
moves or stops anything.

Fields not set fall back to the catalog type's `defaults.yaml`; every
config knob the type defines is validated against its `schema.json`
(JSON Schema 2020-12) and passed to the unit generator. Config is
block-scoped under `config:` — e.g. the shipped `util/echo` type takes
`config: { body: "hello", port: 8080 }`.

### 2. Validate and apply

```console
$ expanse ctl block apply -f web.yaml --dry-run   # validate only, no writes
$ expanse ctl block apply -f web.yaml
created default/web
```

`apply` is idempotent: a second apply with a changed spec is an update
(the strategy's update mode decides how it rolls out — see step 5);
an unchanged apply is a no-op.

### 3. Watch it schedule and run

```console
$ expanse ctl block get web -o json | jq .status
{
  "phase": "RUNNING",
  "placements": [
    { "replicaIndex": 0, "nodeId": "n1", "phase": "RUNNING", "generation": "12" },
    { "replicaIndex": 1, "nodeId": "n2", "phase": "RUNNING", "generation": "12" },
    { "replicaIndex": 2, "nodeId": "n3", "phase": "RUNNING", "generation": "12" }
  ]
}
```

What happened between apply and `RUNNING`:

1. **Scheduling** — the scheduler filtered the node views (ready,
   uncordoned, resources free after overcommit accounting) and scored
   the survivors (spread: even → least-loaded wins). Each replica got
   a distinct node because `antiAffinity: node`.
2. **Desired-state bridge** — each placement became a
   `block-replica:<ns>/<name>/<idx>` resource on its target node.
3. **Agent reconcile** — the target node's agent rendered the nginx
   config from the type schema + defaults and started
   `expanse-block@<...>.service` in a hardened sandbox
   (`DynamicUser`, `ProtectSystem=strict`, `PrivateTmp`, ...).
4. **Promotion** — the agent reported `health=healthy in_sync=true`
   (and, for a VM, that its guest booted: see `docs/VMS.md`);
   the controller promoted each placement and then the block to
   `RUNNING`.

If a replica cannot be placed anywhere, the block goes `PENDING` with
a machine-readable reason (e.g. `InsufficientCPU`) — and `explain`
tells you exactly why:

```console
$ expanse ctl block explain web
web: 3/3 placed (phase RUNNING)
```

On a pending block, `explain` prints the per-node filter verdict and
score breakdown (§7): why each node was rejected, or why the winner
won. Use it before blaming the network.

### 4. Operate

```console
$ expanse ctl block logs default/web              # stream logs (any node, cross-node)
$ expanse ctl block logs default/web --replica 1  # one replica
$ expanse ctl block restart web --replica 0
$ expanse ctl block events web            # lifecycle events with reasons
$ expanse ctl block scale web --replicas 5
```

Scaling is convergent and idempotent: the controller adds or retires
placements one pass at a time until the status matches the spec, and a
repeated scale to the same count changes nothing. Node death is
handled automatically: after the unreachable grace (30 s default), the
placement is marked `LOST`, retired, and a replacement is scheduled —
the same anti-affinity rules apply. A `singleton` block instead waits
for its lease to expire (15 s TTL) so exactly one instance exists at
any moment, even across partitions.

### 5. Update it

Bump the version or change config, then apply again:

```console
$ expanse ctl block apply -f web.yaml     # spec changed: version 1.29
```

With `mode: rolling` the controller updates one replica at a time:
it surges only up to `maxSurge` extra placements, waits for the new
placement to report healthy plus `minReadySeconds`, retires one old
replica (within `maxUnavailable`), and only then proceeds — so at
least `N - maxUnavailable` replicas stay available throughout
(the rolling VM test asserts **zero** failed health probes during the
roll). `mode: recreate` tears down before standing up; the generation
of the block record pins every placement to the exact revision it was
scheduled from, so a mid-roll crash resumes deterministically.

### 6. Delete it

```console
$ expanse ctl block delete web
```

All replica units are stopped and removed, their cgroups emptied, the
desired-state resources and status keys are deleted, and the
resources return to the nodes' free pools. Nothing lingers.

## Block types (the catalog)

Shipped types live in `nix/blocks/<category>/<name>/`, each with:

| File | Role |
|---|---|
| `block.yaml` | type identity (name, version, description, capabilities) |
| `schema.json` | JSON Schema for the type's `config:` section |
| `defaults.yaml` | fallback values for unset config keys |
| `module.nix` | NixOS closure the unit runs from |

The shipped eleven: `util/echo`, `web/nginx`, `web/static-site`,
`web/whoami`, `db/redis`, `db/postgres`, `monitor/node-exporter`,
`ai/ollama`, `share/smb`, `iscsi/target`, `vm/instance`. Inspect them
with:

```console
$ expanse ctl catalog list
$ expanse ctl catalog show web/nginx     # identity + JSON Schema
```

Authoring a new type is those four files plus one entry in
`nix/blocks-flake/flake.nix`; validation, scheduling, lifecycle, and
updates come for free.
