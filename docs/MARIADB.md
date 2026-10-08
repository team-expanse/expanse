# MariaDB

How to deploy a `db/mariadb` block, connect to it, and what a client sees when the node serving it
fails.

## 1. The design, in one paragraph

`db/mariadb` runs one `mariadbd` on a DRBD-backed volume. It is active/passive like `storage/s3`:
the block is `SINGLETON`, it runs only where the volume's DRBD primary is, and clients reach it
through a stable external VIP (`EXPOSE_VIP`). The whole datadir lives on the volume under
`mariadb/`, so the node that takes over after a failover runs InnoDB crash recovery on the same
data. Replication is DRBD's job (synchronous, protocol C), so MariaDB's own replication and Galera
are not used. InnoDB syncs its redo log on every commit (`innodb_flush_log_at_trx_commit=1`) and
keeps the doublewrite buffer on, so every commit a client sees acknowledged survives the crash a
failover is. Contrast `db/postgres` ([`DATABASE.md`](DATABASE.md)), which replicates with the
database's own streaming replication across per-replica volumes.

## 2. Deploying

```yaml
apiVersion: expanse.io/v1
kind: Block
metadata:
  name: appdb
  namespace: default
spec:
  type: db/mariadb
  replicas: 1
  strategy:
    kind: SINGLETON
  resources:
    requests:
      cpu: 500m
      memory: 1Gi
  storage:
    - name: appdb-data
      size: 50Gi
      replication: 3
      mountPath: /var/lib/mariadb
  config:
    port: 13306          # mariadbd's own port; never 3306 (see below)
    rootPassword: change-me-root
    database: app
    user: app
    password: change-me-app
    bufferPool: 512M
  network:
    ports:
      - name: mysql
        port: 3306
        target_port: 13306
        protocol: tcp
        expose: EXPOSE_VIP
    health_check:
      readiness:
        type: PROBE_TCP
        port: 13306
        period_seconds: 2
```

`config.port` must differ from the exposed port: the VIP holder binds `VIP:3306` on the very node
`mariadbd` runs on.

| Key | Default | Meaning |
|---|---|---|
| `rootPassword` | required | Password for `root@localhost` and `root@'%'`, at least 8 characters |
| `database` | none | Database created on start if it is missing |
| `user` | none | Application account with all privileges on `database`; needs `password` and `database` |
| `password` | none | Password for `user` |
| `bufferPool` | `128M` | `innodb_buffer_pool_size`; keep it inside the memory request |
| `maxConnections` | `151` | `max_connections` |
| `port` | `13306` | `mariadbd`'s listen port (`target_port`) |

On first start the block creates the system tables in a side directory and renames it into place,
so a crash mid-install never leaves a half-made datadir. On every start an init file creates the
database and accounts if missing and sets their passwords, so changing `rootPassword` or
`password` and redeploying rotates them. The only root accounts are `root@localhost` and
`root@'%'`. The block deletes every other root account, and every anonymous account, on each start;
these include the passwordless `root@127.0.0.1`, `root@::1` and `root@<hostname>` that
`mariadb-install-db` creates. Renaming `user` adds the new account and leaves the old one in place;
removing `database` does not drop it.

Each node must have `pkgs.mariadb` in `environment.systemPackages`.

## 3. Clients

Connect to `<VIP>:3306` with any MySQL or MariaDB client or driver:

```sh
mariadb -h <VIP> -P 3306 -u app -p app
```

A DSN for Go's `database/sql`: `app:<password>@tcp(<VIP>:3306)/app`.

**Security.** Connections are not encrypted (no TLS is configured). Keep the VIP on a network you
trust. `root@'%'` can log in from anywhere that reaches the VIP, so choose a strong `rootPassword`.

## 4. Failover, from a client's perspective

When the node serving the block dies, the block, its volume's DRBD primary and its VIP move
together to a survivor. Open connections break and in-flight transactions roll back; clients
reconnect to the same endpoint with the same account. A transaction whose `COMMIT` returned is on
the volume. When a connection drops before `COMMIT` returns, the transaction may or may not have
committed. Clients that retry must make the retry idempotent, for example with a unique key.

Measured in the `db-mariadb` VM test (3 nodes, 5 s agent period): the block, primary and VIP
agreed on a survivor 53 s after the serving VM was killed, and a client retrying failed inserts had
its commits acknowledged again within the same second. Every acknowledged row and a
multi-row transaction committed before the failure read back.

## 5. What is not covered

- TLS, and more than one application account or database.
- Read replicas, MariaDB replication and Galera: one block is one `mariadbd`, replicated by DRBD.
- Backups: a crash-consistent volume snapshot ([`BACKUP.md`](BACKUP.md)) is a valid InnoDB
  restore point. For logical dumps, run `mariadb-dump` against the VIP.
