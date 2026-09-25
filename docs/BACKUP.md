# Backup and restore

How to back up a node's cluster state and opaque volume data, and how to restore a destroyed
cluster from that backup. See `.plan/ARCHITECTURE.md` §9 (A35–A36) for the design rationale; this
document is the operator-facing companion.

## 1. The design, in one paragraph

Expanse does not write its own backup engine — it adopts **restic** (`.plan/ARCHITECTURE.md` A35,
chosen over kopia by a real measured adoption test) against any S3-compatible destination. Restic
owns dedupe, encryption and integrity checking; Expanse's own job is making sure the *right paths*
get backed up and restored consistently. Two kinds of state exist: each node's own `dataDir`
(default `/persist/expanse` — cluster identity, join secret, CA, node TLS, the Raft log, and the
generations mechanism's own desired-state history, §4.7) and opaque volume data (LVM thin
snapshots, taken through `expanse ctl volume snapshot`). Restoring `dataDir` is one command,
`expanse cluster restore`; restoring volume data reuses the same restic mechanism, snapshotted and
restored as a flat image (§4 below).

## 2. Backing up a node's cluster state

Back up `dataDir` with restic directly — no Expanse-specific step is needed, since cluster
identity, Raft state and the generations store's history all already live under it
(`.plan/PHASE-08-TASKS.md` D2):

```sh
export RESTIC_REPOSITORY=s3:https://<endpoint>/<bucket>/<node-name>
export RESTIC_PASSWORD=<repository password>
export AWS_ACCESS_KEY_ID=<key id>
export AWS_SECRET_ACCESS_KEY=<secret key>

restic init                       # once, per node's own repository
restic backup /persist/expanse    # repeat on whatever cadence fits (cron, systemd timer, manual)
```

**Each node backs up to its own repository path.** This is what lets the one-command restore
(§3) pull exactly that node's own state back with no tag filtering: `s3://<bucket>/n1`,
`s3://<bucket>/n2`, `s3://<bucket>/n3`, not one shared repository for the whole cluster.

**Cluster-wide consistency:** a backup taken while a node is actively committing to Raft is still
individually correct (restic reads a normal file tree), but backing up all three nodes at slightly
different moments can capture a cluster-wide state that never actually existed together. For a
cluster-wide backup, quiesce first — stop `expansed` on every node, take all three `/persist`
backups, then restart — so every node's backup is provably at the identical Raft position, not
merely close in wall-clock time. This is what
`nix/tests/backup-destroy-rebuild.nix` does, and is the recommended production pattern
(`.plan/PHASE-08-TASKS.md` D5/R2).

## 3. Restoring a destroyed node

Given only the backup credentials above (the `RESTIC_*`/`AWS_*` environment variables — no other
state, no prior cluster membership needed on the fresh disk), restore `dataDir` with the one new
command, **before starting the daemon**:

```sh
expanse cluster restore --data-dir /persist/expanse
systemctl start expansed
```

This restores cluster identity, join secret, CA, node TLS, the Raft log and the generations store
byte-for-byte. Starting the daemon afterward rejoins the node under its **original identity** —
not a fresh one — with no separate re-init or re-join step. If other nodes are still live and
hold quorum, the restored node simply catches up over Raft. If **every** node's state was
destroyed together (the full cluster-loss scenario), restore all of them the same way and start
every daemon together: quorum reforms cold from the restored Raft logs, and the reconciler
converges every node back to the desired state the backup captured — proven end to end by
`nix/tests/backup-destroy-rebuild.nix` (`.plan/PHASE-08-TASKS.md` X6).

## 4. Backing up and restoring volume data

Opaque block-volume data (the actual bytes inside a replicated volume) is a separate concern from
`dataDir` — it lives on LVM thin volumes, not under `/persist`, and is backed up via a snapshot,
not a live read:

```sh
expanse ctl volume snapshot <volume-name> --name <snapshot-name>
```

A thin snapshot LV starts inactive; activate it before reading:

```sh
lvchange --activate y --ignoreactivationskip vg0/<resource>-snap-<snapshot-name>
dd if=/dev/vg0/<resource>-snap-<snapshot-name> of=vol.img bs=1M
restic backup vol.img
```

To restore, `restic restore` the image back out and `dd` it onto the volume's live device (the
same DRBD device every replica shares — a write to it replicates to every UpToDate replica):

```sh
restic restore latest --target ./restored
dd if=./restored/vol.img of=/dev/drbdN bs=1M
```

There is currently no single wrapper command for this side — `expanse ctl volume snapshot` takes
the snapshot, and restic/`dd` move the bytes, mirroring exactly what
`nix/tests/backup-volume-snapshot.nix` and `nix/tests/backup-destroy-rebuild.nix` both do and
verify checksum-equal.

## 5. What is not covered

Backup scheduling (cron, a new block type, automatic cadence) is a deliberately deferred
extension point, not built this phase (`.plan/PHASE-08-TASKS.md` D4) — `restic backup` above is a
manual or externally-scheduled command today. A shipped block's own secret-shaped config fields
(e.g. `iscsi/target`'s `chapPassword`, `db/postgres`'s replication/superuser passwords) are
ordinary plaintext values on the block spec, already covered by the generation backup (§2, X4) —
not V20's separate per-block secrets store (`internal/blocks/validate`'s still-stub
`SecretsExist`), which remains an open TODO from Phase 6 that no shipped block type actually
exercises today (`.plan/PHASE-08-TASKS.md` D3).
