# Jellyfin

How to deploy a `media/jellyfin` block, give it a media library, and what viewers see when the node
serving it fails.

## 1. The design, in one paragraph

`media/jellyfin` runs [Jellyfin](https://jellyfin.org), a media server for movies, shows, music and
photos with apps for browsers, phones and TVs. It is active/passive like `security/vaultwarden`: the
block is `SINGLETON`, it runs only where its volume's DRBD primary is, and clients reach it through a
stable VIP. The volume holds what Jellyfin cannot rebuild: its SQLite database (users, watch
history, sessions), its settings and library definitions, and downloaded metadata and artwork. The
media itself is not on the volume; every node reads it from the same path. The image cache and
transcodes go to the unit's private `/tmp`, so they are never replicated and are rebuilt after a
failover.

## 2. Deploying

```yaml
apiVersion: expanse.io/v1
kind: Block
metadata:
  name: media
  namespace: default
spec:
  type: media/jellyfin
  replicas: 1
  strategy:
    kind: SINGLETON
  resources:
    requests:
      cpu: "2"
      memory: 1Gi
  storage:
    - name: jellyfin-data
      size: 10Gi
      replication: 3
      mountPath: /var/lib/jellyfin
  config:
    publishedServerUrl: https://media.example.com
  network:
    ports:
      - name: http
        port: 80
        target_port: 8096
        protocol: tcp
        expose: EXPOSE_VIP
    health_check:
      readiness:
        type: PROBE_TCP
        port: 8096
        period_seconds: 2
```

Jellyfin refuses to start with less than 2 GiB free for its data, so size the volume above that
(it holds artwork, which grows with the library); the nodes' `/tmp` needs 2 GiB free for its cache.

Each node needs `pkgs.jellyfin` in `environment.systemPackages`; it brings Jellyfin's web client
and its own ffmpeg.

Jellyfin listens on 8096. Open `http://<VIP>/` right after deploying: until its startup wizard is
finished, anyone who reaches Jellyfin can create the administrator. For HTTPS, put a `web/caddy`
block in front and point it at this block's VIP (`expanse ctl block get media`).

| Key | Default | Meaning |
|---|---|---|
| `publishedServerUrl` | none | URL Jellyfin gives clients that find it by LAN discovery |

Everything else (libraries, users, transcoding, plugins) is set in Jellyfin's dashboard and kept on
the volume. The block owns `config/database.xml` and rewrites it on every start (§3).

### Media

Mount the media at the same path on every node that may run the block, for example an NFS export in
each node's NixOS configuration, and add that path as a library in the dashboard. The block runs as
an unprivileged dynamic user, so the files must be world-readable, and it cannot see `/home`.
Jellyfin only reads the media; it does not need write access.

The unit's sandbox does not let Jellyfin list the node's network interfaces, so it logs "Error
obtaining interfaces" at start, and the block starts it with `--nonetchange`. Jellyfin then counts
the private ranges (10/8, 172.16/12, 192.168/16) as the LAN; if yours differs, list its subnets
under *Networking* in the dashboard.

## 3. Failover, from a viewer's perspective

When the node serving the block dies, the block, its volume's DRBD primary and its VIP move together
to a survivor, and Jellyfin starts there on the same volume. Streams in progress stop; the address
stays the same, and apps stay signed in, since their tokens are
in the database on the volume.

Nothing Jellyfin acknowledged is lost. Jellyfin opens SQLite with `synchronous=NORMAL`, under which
a committed change can sit unsynced until the next checkpoint, so a crash can lose it. The block
writes `database.xml` with `syncmode` 2 (`synchronous=FULL`) before every start, and DRBD
replicates each write to the other nodes before it completes. Settings, library definitions and
artwork are written without syncing and are flushed every 2 s.

Measured in the `media-jellyfin` VM test (3 nodes, 5 s agent period): a client completed the startup
wizard, added a movie library from `/srv/media`, streamed the movie, created 20 users, and the
serving VM was killed as soon as the last was acknowledged. The block, its volume primary and its
VIP agreed on a survivor 58 s later in two runs, and Jellyfin answered 135 s after the crash with all 20 users,
the library and the movie; the token issued before the crash still worked, and the survivor saved a
new user. Most of the wait after re-converging is Jellyfin's own start, which took 75–85 s on these
single-CPU VMs on the first start too: it runs ffmpeg capability probes before it serves requests. The
same test with Jellyfin's own `synchronous=NORMAL` lost the last 2 users acknowledged before the
crash.

## 4. What is not covered

- More than one Jellyfin at a time. Jellyfin keeps its state in SQLite on one volume.
- Hardware transcoding. The block's unit cannot open the GPU's render device, so Jellyfin transcodes
  on the CPU; give it CPU accordingly, or use clients that play the media directly.
- HTTPS inside the block; use `web/caddy` in front.
- Serving the media itself; mount it on the nodes (§2).
- Backups: back up the volume with `util/restic-backup` ([`RESTIC-BACKUP.md`](RESTIC-BACKUP.md)).
