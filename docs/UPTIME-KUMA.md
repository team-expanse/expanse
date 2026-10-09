# Uptime Kuma

How to deploy a `monitor/uptime-kuma` block and what users see when the node serving it fails.

## 1. The design, in one paragraph

`monitor/uptime-kuma` runs [Uptime Kuma](https://uptime.kuma.pet), a self-hosted monitoring tool
that checks websites, ports, DNS and more, keeps their history, sends notifications and publishes
status pages. It is active/passive like `security/vaultwarden`: the block is `SINGLETON`, it runs
only where its volume's DRBD primary is, and users reach it through a stable VIP. The volume holds
Uptime Kuma's SQLite database (monitors, heartbeat history, users, notifications, status pages),
its uploaded images and `db-config.json`.

## 2. Deploying

```yaml
apiVersion: expanse.io/v1
kind: Block
metadata:
  name: uptime
  namespace: default
spec:
  type: monitor/uptime-kuma
  replicas: 1
  strategy:
    kind: SINGLETON
  resources:
    requests:
      cpu: 250m
      memory: 256Mi
  storage:
    - name: uptime-kuma-data
      size: 2Gi
      replication: 3
      mountPath: /var/lib/uptime-kuma
  network:
    ports:
      - name: http
        port: 80
        target_port: 3001
        protocol: tcp
        expose: EXPOSE_VIP
    health_check:
      readiness:
        type: PROBE_TCP
        port: 3001
        period_seconds: 2
```

Each node needs `pkgs.uptime-kuma` in `environment.systemPackages`.

Open `http://<VIP>/` right after deploying: until the first account is created, anyone who reaches
Uptime Kuma can create it. For HTTPS, put a `web/caddy` block in front and point it at this block's
VIP (`expanse ctl block get uptime`). Heartbeat history grows with the number of monitors and how
long it is kept (*Settings → Monitor History*); size the volume for it.

| Key | Default | Meaning |
|---|---|---|
| `port` | 3001 | Uptime Kuma's HTTP port, set as `target_port` |

Everything else (monitors, notifications, status pages, users) is set in Uptime Kuma's web UI and
kept on the volume. The block always uses SQLite: it sets `UPTIME_KUMA_DB_TYPE=sqlite`, so the
first-run page that asks for a database never shows.

Monitors run from whichever node serves the block, so every node must be able to reach what is
monitored.

## 3. Failover, from a user's perspective

When the node serving the block dies, the block, its volume's DRBD primary and its VIP move together
to a survivor, and Uptime Kuma starts there on the same volume. Checks pause until it is back; the
address stays the same and browsers stay signed in, since the key that signs their session is in
the database.

Nothing Uptime Kuma acknowledged is lost. Uptime Kuma opens SQLite with `synchronous = NORMAL` and
has no setting to change it; under it, a committed change can sit unsynced until the next
checkpoint, so a crash can lose it. The block starts Node.js with a small preload
(`--require`) that turns every `PRAGMA synchronous` assignment Uptime Kuma's SQLite driver runs into
`synchronous = FULL`, and logs `expanse: SQLite synchronous pragmas forced to FULL`. A test
(`checks.uptime-kuma-sync-full`) runs the preload against the packaged driver, so an upgrade that
bypasses it fails the build. DRBD replicates each write to the other nodes before it completes.
Uploaded images and `db-config.json` are written without syncing and are flushed every 2 s.

Measured in the `monitor-uptime-kuma` VM test (3 nodes, 5 s agent period): a client created the
administrator, added an HTTP monitor of a web server outside the cluster that reported UP, added 20
push monitors one acknowledged at a time, and the serving VM was killed as soon as the last was
acknowledged. The block, its volume primary and its VIP agreed on a survivor 72 s later in two runs, and
Uptime Kuma answered 106 s after the crash with all 21 monitors; the session token issued before the
crash still worked, the HTTP monitor reported UP again, and the survivor saved a new monitor. The
same test without the preload's override, under Uptime Kuma's own `synchronous = NORMAL`, lost all
20 push monitors.

## 4. What is not covered

- More than one Uptime Kuma at a time. Uptime Kuma keeps its state in SQLite on one volume.
- MariaDB as the database: the block always uses SQLite on its volume.
- Monitors that need a browser (*Real Browser*): nodes do not ship Chromium.
- HTTPS inside the block; use `web/caddy` in front.
- Backups: back up the volume with `util/restic-backup` ([`RESTIC-BACKUP.md`](RESTIC-BACKUP.md)).
