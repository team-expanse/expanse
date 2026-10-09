# Vaultwarden

How to deploy a `security/vaultwarden` block, connect Bitwarden apps to it, and what users see when
the node serving it fails.

## 1. The design, in one paragraph

`security/vaultwarden` runs [Vaultwarden](https://github.com/dani-garcia/vaultwarden), a server
for the Bitwarden password manager apps, browser extensions and web vault. It is active/passive like
`dev/forgejo`: the block is `SINGLETON`, it runs only where its volume's DRBD primary is, and
clients reach it through a stable VIP. Everything Vaultwarden keeps lives on the volume: the SQLite
database, the RSA key that signs login tokens, attachments, Sends and settings saved from the admin
page. Vaults are encrypted on each client with the user's master password; the server never sees it.

## 2. Deploying

```yaml
apiVersion: expanse.io/v1
kind: Block
metadata:
  name: vault
  namespace: default
spec:
  type: security/vaultwarden
  replicas: 1
  strategy:
    kind: SINGLETON
  resources:
    requests:
      cpu: 200m
      memory: 128Mi
  storage:
    - name: vaultwarden-data
      size: 2Gi
      replication: 3
      mountPath: /var/lib/vaultwarden
  config:
    domain: https://vault.example.com
    signupsAllowed: true
    adminToken: $argon2id$v=19$m=65540,t=3,p=4$...
  network:
    ports:
      - name: http
        port: 80
        target_port: 18000
        protocol: tcp
        expose: EXPOSE_VIP
    health_check:
      readiness:
        type: PROBE_TCP
        port: 18000
        period_seconds: 2
```

Browsers open the web vault only over HTTPS, since it needs the Web Crypto API, so put a
`web/caddy` block in front, point `vault.example.com` at Caddy's VIP, have Caddy proxy to this
block's VIP (`expanse ctl block get vault`), and set `domain` to the `https://` address. Plain HTTP
reaches only the API, and it sends every login over the network unencrypted.

Each node needs `pkgs.vaultwarden` and, for the web vault, `pkgs.vaultwarden.webvault` in
`environment.systemPackages`. Without the web vault, the block serves only the API that apps and
extensions use.

| Key | Default | Meaning |
|---|---|---|
| `domain` | required | URL users and apps reach Vaultwarden at; emails and links are built from it |
| `signupsAllowed` | `false` | Let anyone who reaches Vaultwarden create an account |
| `adminToken` | none | Turns on the `/admin` page. Use the Argon2 string `vaultwarden hash` prints; a plain token works but is logged as insecure |
| `port` | `18000` | Vaultwarden's HTTP port (`target_port` of 80) |
| `settings` | none | Extra Vaultwarden environment variables, applied after the block's own |

`settings` takes any variable from Vaultwarden's
[`.env.template`](https://github.com/dani-garcia/vaultwarden/blob/main/.env.template), such as a
mail server for invitations, or sign-ups open to one email domain only:

```yaml
    settings:
      SIGNUPS_DOMAINS_WHITELIST: example.com
      SMTP_HOST: smtp.example.com
      SMTP_FROM: vault@example.com
      SMTP_PORT: 587
      SMTP_SECURITY: starttls
```

The block sets `DATA_FOLDER` to the volume and `DATABASE_CONN_INIT` to make SQLite sync every
commit (§3); `settings` can override either, but then a failover may lose data. Settings saved from
the admin page are kept in `config.json` on the volume and take precedence over the manifest.
nixpkgs builds Vaultwarden with SQLite only, so `DATABASE_URL` cannot point at `db/postgres` or
`db/mariadb`.

## 3. Failover, from a user's perspective

When the node serving the block dies, the block, its volume's DRBD primary and its VIP move
together to a survivor, and Vaultwarden starts there on the same volume. Requests in flight fail; the
address stays the same. Logged-in apps stay logged in: their tokens are signed with the RSA key on
the volume.

Nothing Vaultwarden acknowledged is lost. Vaultwarden itself opens SQLite with
`synchronous=NORMAL`, under which a committed change can sit unsynced until the next checkpoint, so a
crash can lose it. The block replaces that with `synchronous=FULL`, and DRBD replicates each write to
the other nodes before it completes. Attachments, Sends and `config.json` are written without
syncing and are flushed every 2 s.

Measured in the `security-vaultwarden` VM test (3 nodes, 5 s agent period): a client saved 20 vault
items and the serving VM was killed as soon as the last was acknowledged. The block, its volume
primary and its VIP agreed on a survivor 54 s and 65 s later in two runs, and Vaultwarden answered at once with all 20
items; the login token issued before the crash still worked, and the survivor saved a new item. The
same test with Vaultwarden's own `synchronous=NORMAL` lost the last item acknowledged before the crash.

## 4. What is not covered

- More than one Vaultwarden at a time. Vaultwarden keeps its state in SQLite on one volume.
- HTTPS inside the block; use `web/caddy` in front.
- Push notifications to mobile apps, which need an installation ID from Bitwarden (`PUSH_*` in
  `settings`); the test does not cover them.
- Backups: back up the volume with `util/restic-backup` ([`RESTIC-BACKUP.md`](RESTIC-BACKUP.md)).
