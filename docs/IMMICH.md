# Immich

How to deploy a `media/immich` block and what users see when the node serving it fails.

## 1. The design, in one paragraph

`media/immich` runs [Immich](https://immich.app), a self-hosted photo and video library with
mobile backup, albums, sharing, and (with machine learning) smart search and face recognition.
It is active/passive like `security/vaultwarden`: the block is `SINGLETON`, it runs only where its
volume's DRBD primary is, and users reach it through a stable VIP. The block runs Immich's whole
stack itself, with every piece's state on the one volume: a private Postgres 18 with the
VectorChord extension (reachable only over a Unix socket), Redis for Immich's job queue, the
optional machine-learning service on loopback, and the Immich server. The volume holds the
uploads, thumbnails and transcoded videos, the database, the job queue and the downloaded models.

## 2. Deploying

```yaml
apiVersion: expanse.io/v1
kind: Block
metadata:
  name: photos
  namespace: default
spec:
  type: media/immich
  replicas: 1
  strategy:
    kind: SINGLETON
  resources:
    requests:
      cpu: "2"
      memory: 4Gi
  storage:
    - name: immich-data
      size: 500Gi
      replication: 3
      mountPath: /var/lib/immich
  network:
    ports:
      - name: http
        port: 80
        target_port: 2283
        protocol: tcp
        expose: EXPOSE_VIP
    health_check:
      readiness:
        type: PROBE_TCP
        port: 2283
        period_seconds: 2
```

Each node that may run the block needs these in `environment.systemPackages`:

```nix
pkgs.immich
pkgs.immich.machine-learning  # unless machineLearning is false
pkgs.redis
(pkgs.postgresql_18.withPackages (ps: [ ps.pgvector ps.vectorchord ]))
```

The Postgres package is a superset of `pkgs.postgresql_18`, so `db/postgres` blocks on the same
node keep working with it; ship it in place of `pkgs.postgresql_18`, not beside it. The block
refuses to start, naming the package, when the `postgres` on the PATH lacks VectorChord.

Open `http://<VIP>/` right after deploying: until the administrator account is created, anyone who
reaches Immich can create it. For HTTPS, which the mobile apps want, put a `web/caddy` block in
front and point it at this block's VIP (`expanse ctl block get photos`). Size the volume for the
library: originals, plus thumbnails and transcoded videos (roughly a further 10 to 20 percent),
plus the database.

| Key | Default | Meaning |
|---|---|---|
| `port` | 2283 | Immich's HTTP port, set as `target_port` |
| `machineLearning` | `true` | Run the machine-learning service for smart search, face recognition and OCR |

Everything else (users, libraries, storage template, job settings) is set in Immich's web UI and
kept in its database on the volume. The machine-learning service downloads its models from
Hugging Face the first time each is used and keeps them on the volume, so nodes need internet
access for it. With `machineLearning: false`, also turn machine learning off in *Administration →
Settings → Machine Learning*, or every new photo logs a failed job.

On first start, Immich imports its reverse-geocoding data into the database before it runs any
job; that took 140 s in the VM test. Uploads are accepted meanwhile and processed afterwards.

## 3. Failover, from a user's perspective

When the node serving the block dies, the block, its volume's DRBD primary and its VIP move together
to a survivor, and Immich starts there on the same volume. Uploads and browsing pause until it is
back; the address stays the same, and the apps stay signed in, since sessions are in the database.

Nothing Immich acknowledged is lost:

- **Uploads.** Immich writes each upload with `flush`, so the file is synced before the database
  records it, and Postgres syncs every commit before it acknowledges it (the block passes
  `fsync`, `synchronous_commit` and `full_page_writes` on the command line, so a stray setting
  cannot turn them off).
- **Queued jobs.** Thumbnails, metadata, video transcoding and machine learning run as jobs queued
  in Redis. The block runs Redis with `appendfsync always`, so a job queued before the upload was
  acknowledged survives the crash and runs on the survivor. Without that, those jobs are lost,
  and their photos stay without thumbnails until someone reruns the jobs by hand.

DRBD replicates each write to the other nodes before it completes.

Measured in the `media-immich` VM test (3 nodes, 5 s agent period, `machineLearning: false` since
the test VMs have no internet for the models): a client created the
administrator, uploaded a photo that got its thumbnail, paused metadata extraction, uploaded 20
photos one acknowledged at a time, and the serving VM was killed as soon as the last was
acknowledged, with all 20 jobs waiting in the queue. The block, its volume primary and its VIP
agreed on a survivor 125 s and 114 s later in two runs, and Immich answered 126 s and 115 s after
the crash with all 21 photos,
each original byte for byte; the token issued before the crash still worked, all 20 queued jobs ran
and every photo got its thumbnail, and the survivor took a new upload. The same test with Redis
keeping nothing on the volume kept every photo but lost all 20 jobs: none of those photos got a
thumbnail.

## 4. What is not covered

- More than one Immich at a time. Immich keeps its state in one database on one volume.
- An external database or Redis: the block always runs its own, on its volume.
- Hardware-accelerated transcoding or machine learning: no GPU is passed to the block.
- External libraries on other storage: only what is on the volume fails over with it.
- HTTPS inside the block; use `web/caddy` in front.
- Failover with machine learning running: the VM test runs without it, and the machine-learning
  service was checked only to start and answer with the block's settings.
- Backups: back up the volume with `util/restic-backup` ([`RESTIC-BACKUP.md`](RESTIC-BACKUP.md)).
