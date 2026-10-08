# Forgejo

How to deploy a `dev/forgejo` block, reach it over HTTP and SSH, and what users see when the node
serving it fails.

## 1. The design, in one paragraph

`dev/forgejo` runs [Forgejo](https://forgejo.org/), a Git forge with repositories, issues, pull
requests and packages. It is active/passive like `db/mariadb`: the block is `SINGLETON`, it runs
only where its volume's DRBD primary is, and clients reach it through a stable VIP that serves both
HTTP and Forgejo's built-in SSH server. Everything Forgejo keeps lives on the volume: repositories,
the SQLite database, LFS objects, attachments, the SSH host key and the secrets that sign sessions
and tokens. The block writes `app.ini` on every start and creates the administrator account on the
first one.

## 2. Deploying

```yaml
apiVersion: expanse.io/v1
kind: Block
metadata:
  name: forge
  namespace: default
spec:
  type: dev/forgejo
  replicas: 1
  strategy:
    kind: SINGLETON
  resources:
    requests:
      cpu: 500m
      memory: 512Mi
  storage:
    - name: forgejo-data
      size: 20Gi
      replication: 3
      mountPath: /var/lib/forgejo
  config:
    rootURL: http://git.example.com/
    adminUser: gitadmin
    adminPassword: change-me-please
    adminEmail: ops@example.com
  network:
    ports:
      - name: http
        port: 80
        target_port: 13000
        protocol: tcp
        expose: EXPOSE_VIP
      - name: ssh
        port: 2222
        target_port: 12222
        protocol: tcp
        expose: EXPOSE_VIP
    health_check:
      readiness:
        type: PROBE_TCP
        port: 13000
        period_seconds: 2
```

Point `git.example.com` at the block's VIP (`expanse ctl block get forge`). Clone URLs then read
`http://git.example.com/<owner>/<repo>.git` and `ssh://git@git.example.com:2222/<owner>/<repo>.git`.
SSH is exposed on 2222, not 22: the VIP holder binds the exposed port on a node whose own `sshd`
already listens on 22. `port` and `sshPort` must differ from the exposed ports for the same reason.
For HTTPS, put a `web/caddy` block in front and set `rootURL` to the `https://` address.

| Key | Default | Meaning |
|---|---|---|
| `rootURL` | required | URL users browse to; links and clone URLs are built from it |
| `adminUser` | required | Administrator, created on first start; Forgejo reserves some names, such as `admin` |
| `adminPassword` | required | The administrator's password, at least 8 characters; applied on every start |
| `adminEmail` | required | The administrator's email address |
| `allowRegistration` | `false` | Let anyone who reaches Forgejo register an account |
| `port` | `13000` | Forgejo's HTTP port (`target_port` of 80) |
| `sshPort` | `12222` | The built-in SSH server's port (`target_port` of 2222) |
| `publicSSHPort` | `2222` | SSH port shown in clone URLs: the port exposed on the VIP |
| `settings` | none | Extra `app.ini` settings by section, applied after the block's own |

`settings` takes any [Forgejo setting](https://forgejo.org/docs/latest/admin/config-cheat-sheet/),
such as a mailer or an external database:

```yaml
    settings:
      database:
        DB_TYPE: mysql
        HOST: 10.0.0.40:3306
        NAME: forgejo
        USER: forgejo
        PASSWD: secret
      mailer:
        ENABLED: true
        SMTP_ADDR: smtp.example.com
```

A `db/mariadb` block's VIP can serve as the external database; the VM test covers only SQLite.
The block hands `adminPassword` to Forgejo's command line at start, so it shows briefly in the
serving node's process list. Settings the block writes itself, such as
`ROOT_URL` or `[repository] ROOT`, can be overridden too; moving state off the volume means a
failover no longer carries it. The SSH user in clone URLs is always `git`. Each node must have
`pkgs.forgejo` in `environment.systemPackages`; it brings its own `git`.

## 3. Failover, from a user's perspective

When the node serving the block dies, the block, its volume's DRBD primary and its VIP move
together to a survivor, and Forgejo starts there on the same volume. Pushes, clones and page loads
in flight fail; clients retry against the same address and see the same SSH host key, so `ssh`
raises no warning. Logged-in users stay logged in.

Nothing Forgejo acknowledged is lost. The block sets git's `core.fsync=all`, so a push is on the
volume before `git push` reports success, and SQLite syncs every transaction. DRBD replicates each
write to the other nodes before it completes. Files Forgejo writes without syncing, such as
sessions and avatars, are flushed every 2 s; a session created in the last 2 s before a crash may
need a new login.

Measured in the `dev-forgejo` VM test (3 nodes, 5 s agent period): the serving VM was killed
immediately after an issue was filed and a push was acknowledged. The block, its volume primary
and its VIP agreed on a survivor 60 s later, and Forgejo answered at once. The clone had every
pushed commit and passed `git fsck --strict`, the issue was there, the SSH host key was unchanged,
the web session made before the crash still worked, and the survivor accepted a new push.

## 4. What is not covered

- More than one Forgejo serving the same repositories. Forgejo needs shared storage and an
  external database for that; the shipped block keeps both on its own volume.
- Forgejo Actions runners. Forgejo serves Actions, but jobs run on separate runners, which
  are not part of this block.
- HTTPS inside the block; use `web/caddy` in front.
- SSH on port 22 of the VIP.
