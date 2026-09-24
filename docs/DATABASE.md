# Database

How to deploy a `db/postgres` block, what failover looks like from a client's perspective, and
where backup integration hooks in. See `.plan/ARCHITECTURE.md` §9 (A28–A31) for the design
rationale; this document is the operator-facing companion.

## 1. The design, in one paragraph

`db/postgres` is a plain active-active block (`replicas: N`, no `strategy.kind`) — unlike
`share/smb`/`iscsi/target`'s `SINGLETON` shape, each replica runs on its own node with its own
independent volume (`replication: 1`; PostgreSQL's own streaming replication is the redundancy
mechanism, not DRBD). Which replica is primary is decided by `pgha`, a lease-gated controller built
directly on this project's existing Raft lease rather than an adopted cluster manager (Patroni,
repmgr, pg_auto_failover — all considered and rejected, `ARCHITECTURE.md` A28): each replica
competes for an instance-scoped lease, the winner becomes primary and every other replica clones
from it via `pg_basebackup`, and on primary loss a surviving replica's own lease acquisition
promotes it live via `pg_promote()` — no restart. A client reaches whichever replica is currently
primary through a single stable `EXPOSE_VIP` address; the load balancer's `PrimaryOnly` routing
mode (`internal/agent/lb.go`) filters the pool to the one replica `pgha` currently marks primary,
not a round-robin across all of them, since a write landing on a streaming replica would fail or go
to the wrong place.

## 2. Deploying a replica set

```yaml
apiVersion: expanse.io/v1
kind: Block
metadata:
  name: pg
  namespace: default
spec:
  type: db/postgres
  replicas: 3
  placement:
    antiAffinity: ANTI_AFFINITY_NODE
  resources:
    requests:
      cpu: 200m
      memory: 256Mi
  storage:
    - name: pgdata
      size: 10Gi
      replication: 1              # one independent volume per replica (D3) -- NOT DRBD failover
      mountPath: /var/lib/postgresql-data
  config:
    port: 5432
    database: app
    replicationPassword: <shared secret>   # the internal 'replicator' role standbys use
    superuserPassword: <shared secret>     # the postgres superuser, over TCP
    sharedBuffers: 256MB
    maxWalSenders: 10
    maxReplicationSlots: 10
  network:
    ports:
      - name: pg
        port: 5432
        target_port: 5432
        protocol: tcp
        expose: EXPOSE_VIP
    health_check:
      readiness:
        type: PROBE_TCP
        port: 5432
        period_seconds: 2
```

`storage[0].replication` must be `1` — this is not `share/smb`'s "3-way replicated volume that
fails over," it is 3 independent volumes, each local to its own replica, kept in sync by
PostgreSQL itself. `replicas` and `replication` are independent knobs here in a way no prior block
type needed to distinguish. `maxWalSenders`/`maxReplicationSlots` should exceed the replica count
with headroom for `pg_basebackup` connections during a rejoin (§4). `pg_hba.conf` trusts
`0.0.0.0/0` for both replication and general connections — unlike `share/smb`/`iscsi/target`,
there is no CIDR-level restriction, so `replicationPassword`/`superuserPassword` are the only
access control; a node-to-node replication dial resolves the peer's address the same way the
cluster's own Raft peers do (`internal/agent`'s `lookupNodeIP`, the node join record's
`raft_addr`), not a separate overlay.

Apply it the same way as any other block:

```sh
expanse ctl block apply -f pg.yaml
```

## 3. Connecting a client

Point a client at the block's `EXPOSE_VIP` address and the `port` configured above. **Validated
client/driver: `psql`/libpq only** (R2) — the VIP mechanism itself is protocol-agnostic (every
prior phase's failover used the same shape), but no other PostgreSQL client library or connection
pooler has been exercised against it by this project's VM tests; treat anything else as unverified
until someone does.

```sh
PGPASSWORD=<superuserPassword> psql -h <VIP> -p 5432 -U postgres -d app
```

There is no discovery step and no multi-host connection string to construct — unlike libpq's own
`target_session_attrs=read-write` alternative (`PHASE-05-TASKS.md` D2), routing to the current
primary happens entirely server-side, so a plain single-host connection string always reaches a
writable primary.

## 4. Failover, from a client's perspective

When the node running the current primary is lost, a surviving replica's own lease acquisition
promotes it live (`pg_promote()`, no restart), and the VIP moves to it. The client's *existing*
connection breaks — PostgreSQL protocol connections are stateful TCP, unlike a stateless HTTP
client that can simply retry against the same address — and must be re-established; there is no
transparent mid-connection failover the way `open-iscsi`'s own session recovery gives
`iscsi/target`. A pooler or application retry loop dialing the same VIP address again after a
failed query picks up the new primary automatically.

**No acknowledged write is lost.** `synchronous_standby_names = 'ANY 1 (*)'` means a commit does
not return to the client until at least one standby has confirmed receipt, so whichever replica
`pgha` promotes already has every write the client believed succeeded — measured directly
(`nix/tests/db-postgres-failover.nix`, `nix/tests/db-postgres-vertical-slice.nix`): a precisely
tracked writer plus sustained `pgbench` load run continuously through a hard primary kill, and
every transaction the writer received a commit acknowledgement for is independently confirmed
still present afterward. This tolerates one standby being briefly unreachable without blocking
every write, at the cost of not guaranteeing a *second*, simultaneously-lagging standby also has
every acked write — a known gap, not a byzantine-safe guarantee (`ARCHITECTURE.md` D1's R1).

After a promotion, the promoted primary is verified corruption-free two ways
(`nix/tests/db-postgres-recovery.nix`, `nix/tests/db-postgres-vertical-slice.nix`): `amcheck`'s
`bt_index_check` runs live, with no downtime, against every B-tree index; `pg_checksums`
additionally verifies every data page, which needs the cluster briefly stopped (an operator-driven
check, not something this project runs automatically in production).

## 5. Old-primary rejoin

The old primary's node, once it comes back, does **not** stay a permanently diverged standalone
primary and does **not** require a manual `pg_rewind`. `pgha`'s own reclaim path discovers that
another replica already holds the primary lease for real and rewrites this node's role file to
`replica`; `expanse-block-run`'s `watchForDemotion` watcher then stops postgres, wipes this node's
own `PGDATA`, and lets the unit's own `Restart=on-failure` re-bootstrap it fresh — a full
`pg_basebackup` from the new primary, not an incremental resync. This is simpler than `pg_rewind`
(no timeline-divergence reconciliation logic to get right) at the cost of a full re-copy every
time, judged the right trade for this project's own lease-loss retry design
(`ARCHITECTURE.md` A31). Measured: the rejoined node reports itself in recovery and streaming
(`pg_stat_replication` on the new primary shows it) within a few minutes of restart, and a fresh
write made after rejoin is confirmed to actually reach it, not just a stale pre-rejoin snapshot.

**Split-brain prevention** relies on the same single-holder guarantee this project's Raft lease
already provides for DRBD promotion and every VIP holder — two replicas cannot simultaneously hold
the primary lease. Verified against a real network partition, not just a clean node kill
(`nix/tests/db-postgres-partition.nix`, `PHASE-05-TASKS.md` R3): isolating the primary's node from
its peers (while leaving it reachable to a direct client, bypassing the VIP) shows a direct write
against it is never acknowledged, and once the partition heals it rejoins with zero divergence, the
same as after a kill. In practice this project's own general "no quorum" fail-safe — any node that
loses Raft quorum tears down every block workload it runs, including postgres, well inside the
lease's own guard band — closes the common case before `pgha`'s own lease-loss self-fencing
(mirroring `vip.Holder`'s proven pattern) is even reached; the latter remains a real, independently
tested defense for the narrower case of a lease reassigned while this node's own store access is
otherwise healthy.

## 6. Where replication and WAL state live

Each replica's data directory is `<mountPath>/pgdata`, on that replica's own independent volume —
inspect it directly (e.g. `pg_controldata`, `pg_waldump`) the same way any standalone PostgreSQL
install would be inspected; nothing about its layout is expanse-specific. Streaming replication
uses physical replication slots (`max_replication_slots`, one per standby, created automatically by
the primary-side bootstrap), so WAL needed by a temporarily-disconnected standby is retained rather
than recycled out from under it — bounded by disk space, not time, so a standby down for a very
long while can still exhaust the primary's WAL retention.

## 7. Backup integration hooks (X5)

**Documented, not built** — `ROADMAP.md` defers actual backup/restore to Phase 8; this phase's
obligation is narrower: confirm the integration points Phase 8 needs are reachable.

- **A triggerable base backup:** `pg_basebackup` (shipped in every replica's own image,
  `pkgs.postgresql_18`) can be run against any replica's live `pgdata` directory at any time, the
  same tool this project's own rejoin path (§5) already depends on internally. A backup agent needs
  only network reach to a replica's `port` and the `replicationPassword`/superuser credentials
  already configured.
- **A WAL archiving destination:** `postgresql.conf` and `pg_hba.conf` are written once, at a
  replica's first-ever bootstrap (gated on `PGDATA` still being empty), and are **not** regenerated
  by this project on any later restart. This means `archive_mode`/`archive_command` can be added by
  hand-editing `<mountPath>/pgdata/postgresql.conf` on the current primary and reloading
  (`pg_ctl reload` or `SELECT pg_reload_conf()`) today, without any expanse-side code change; it is
  not currently exposed as a `spec.config` knob, since no destination exists yet for it to point at
  (Phase 8's job). `wal_level = replica` is already set, the prerequisite either mechanism needs.
- **Not covered here:** retention policy, restore procedure, and a `spec.config`-level knob for
  archiving are explicitly Phase 8 scope, not rediscovered or partially built ahead of it.

## 8. Vertical-slice verification

`nix/tests/db-postgres-vertical-slice.nix` runs `pgbench` and a precisely-tracked ack writer
continuously through a hard primary kill, then — against the **same** promoted primary from the
**same** run — confirms zero corruption (`amcheck` + `pg_checksums`) and drives the old primary's
node through a full rejoin as a fresh streaming replica, verified with a real post-rejoin write.
This is the closest exercise of X2 through X4 together this project runs, short of Phase 8's actual
backup/restore work. `nix/tests/db-postgres-partition.nix` covers D5 (split-brain) the same way,
under a real network partition instead of a kill (§5).
