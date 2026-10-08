# Restic backups

How to back up block volumes on a schedule with a `util/restic-backup` block, how to see that the
backups are running, and how to restore a volume from them.

## 1. The design, in one paragraph

`util/restic-backup` runs [restic](https://restic.net/) on every node (`DAEMONSET`). Each replica
backs up only the listed volumes whose DRBD primary is on its own node, so exactly one node backs up
each volume, and after a failover the survivor's replica takes over without any action. A backup
takes an LVM thin snapshot of the volume, streams the volume's bytes from it into the restic
repository and drops the snapshot. The snapshot is crash-consistent: restoring it is like
restarting the block after a power cut, which every shipped block with a volume survives (their
failover tests crash the serving node). restic deduplicates and encrypts, so each backup uploads
only the chunks that changed. When the next backup is due is read from the repository itself, so a
replica that restarts or moves keeps the schedule.

## 2. Deploying

```yaml
apiVersion: expanse.io/v1
kind: Block
metadata:
  name: backup
  namespace: default
spec:
  type: util/restic-backup
  strategy:
    kind: DAEMONSET
  resources:
    requests:
      cpu: 100m
      memory: 256Mi
  config:
    repository: s3:https://s3.example.com/expanse-backups
    password: a-long-repository-password
    env:
      AWS_ACCESS_KEY_ID: GK...
      AWS_SECRET_ACCESS_KEY: ...
    volumes:
      - appdb/appdb-data
      - forge/forgejo-data
    interval: 24h
    keep:
      daily: 7
      weekly: 4
      monthly: 6
  network:
    health_check:
      readiness:
        type: PROBE_HTTP
        path: /healthz
        port: 18900
        period_seconds: 30
```

The first backup of each volume runs as soon as the block starts; the repository is created if it
does not exist. Each node needs `pkgs.restic` in `environment.systemPackages`.

| Key | Default | Meaning |
|---|---|---|
| `repository` | required | Any [restic repository](https://restic.readthedocs.io/en/stable/030_preparing_a_new_repo.html), such as `s3:https://host/bucket` or `sftp:user@host:/path` |
| `password` | required | Encrypts the repository. Keep a copy outside the cluster: without it the backups cannot be read |
| `env` | none | Extra environment for restic, such as the S3 credentials; `RESTIC_REPOSITORY` and `RESTIC_PASSWORD*` are set by the block |
| `volumes` | required | `<block>/<storage>` (a storage entry of a block in this namespace) or a volume name from `expanse ctl volume list` |
| `interval` | `24h` | Time between backups of each volume, at least `1m` |
| `keep` | daily 7, weekly 4, monthly 6 | Backups kept per volume, as `restic forget --keep-*` counts: `last`, `hourly`, `daily`, `weekly`, `monthly`, `yearly` |
| `port` | `18900` | Each replica's status endpoint |

A `storage/s3` block in the same cluster works as the repository, but it shares the cluster's fate;
use storage outside the cluster for backups you need after losing it.

## 3. Seeing what happened

Each replica serves its view on `port`: `GET /` returns, per volume, whether its primary is on this
node, the last backup and the last error; `/healthz` answers 503, naming the error, while a volume
whose primary is here cannot be backed up, which the readiness probe above checks. The replica's
log says when each backup starts and ends:

```console
$ curl -s http://<node>:18900/
{"blk-default-appdb-appdb-data":{"primary":true,"lastBackup":"2026-10-08T03:00:12Z","lastAttempt":"2026-10-08T03:00:09Z"}}
$ expanse ctl block logs default/backup
```

Each volume's backups are restic snapshots whose host is the volume name, so restic lists them
directly:

```console
$ restic snapshots --host blk-default-appdb-appdb-data
```

## 4. Restoring a volume

A backup is the volume's whole device as one file, `<volume>.img`. To put it back, stop the block
that uses the volume, write the image onto the volume's DRBD device on its primary, and start the
block again. Deleting a block keeps its volumes:

```console
$ expanse ctl block delete appdb
$ expanse ctl volume inspect blk-default-appdb-appdb-data      # "primary:" names the node to use
# on that node, wait until the agent has unmounted the volume (this prints nothing):
$ findmnt /var/lib/expanse/volumes/<volume id>/mnt
# then, with the repository's RESTIC_* and credential variables set:
$ restic dump --host blk-default-appdb-appdb-data latest /blk-default-appdb-appdb-data.img \
    | dd of=$(drbdadm sh-dev <volume id>) bs=1M iflag=fullblock oflag=direct
$ expanse ctl block apply -f appdb.yaml
```

Never write while the volume is mounted: the agent unmounts it once the block's process has exited,
and writing under a mounted filesystem corrupts it. The write goes through DRBD, so every replica
receives it. Replace `latest` with a snapshot ID from
`restic snapshots` to restore an older backup. Pause the backup block (`expanse ctl block delete
backup`) first if a backup could start while you restore.

Measured in the `util-restic-backup` VM test (3 nodes, `interval: 1m`, a `db/mariadb` block on a
256 MiB volume, garage as the repository): only the primary's replica backed the volume up, a second
backup carried a row committed after the first, and after the primary's node was crashed the
survivor's replica made the next backup within 85 s of the crash, re-convergence included. `keep.last: 3` left three backups
after five. Restoring the latest backup as above brought MariaDB back with exactly the rows that
backup held.

## 5. What is not covered

- Thick volumes, on nodes configured without a thin pool: only thin volumes can be snapshotted.
- File-level restores: a backup is a device image. Restore it into a scratch volume and mount that
  to pick out files.
- Application-consistent backups. A snapshot is crash-consistent; a database restores the way it
  recovers from a crash.
- A one-command restore; §4 is manual.
- Cluster state (`/persist/expanse`) and generations, which [`BACKUP.md`](BACKUP.md) covers.
