# NFS

How to deploy a `share/nfs` block, mount it, and what a client sees when the node serving it
fails.

## 1. The design, in one paragraph

`share/nfs` exports one directory of a DRBD-backed volume over NFSv4.1 (and 4.2) using
[NFS-Ganesha](https://github.com/nfs-ganesha/nfs-ganesha), a userspace NFS server. It is
active/passive like `share/smb`: the block is `SINGLETON`, it runs only where the volume's DRBD
primary is, and it is reached through a stable external VIP (`EXPOSE_VIP`). Ganesha's
client-recovery records live on the volume itself (`.nfs-state/recovery`), so when the block fails
over they move with the data, and the new server starts a grace period in which clients reclaim
their opens and locks. Ganesha is used instead of the kernel's nfsd because the kernel server has
one export table, one grace period and one recovery directory per host. Each Ganesha instance has
its own, so two exports can share a node.

## 2. Deploying an export

```yaml
apiVersion: expanse.io/v1
kind: Block
metadata:
  name: files
  namespace: default
spec:
  type: share/nfs
  replicas: 1
  strategy:
    kind: SINGLETON
  resources:
    requests:
      cpu: 100m
      memory: 256Mi
  storage:
    - name: files-data
      size: 100Gi
      replication: 3
      mountPath: /mnt/files
  config:
    port: 12049        # Ganesha's own port; never 2049 (see below)
    pseudo: /share     # clients mount <VIP>:/share
    path: share        # exported directory inside the volume
    squash: root       # root | none | all
  network:
    ports:
      - name: nfs
        port: 2049
        target_port: 12049
        protocol: tcp
        expose: EXPOSE_VIP
    health_check:
      readiness:
        type: PROBE_TCP
        port: 12049
        period_seconds: 2
```

`config.port` must differ from the exposed port: the VIP holder binds `VIP:2049` on the very node
Ganesha runs on. Every config key is optional:

| Key | Default | Meaning |
|---|---|---|
| `port` | `12049` | Ganesha's TCP listen port (`target_port`) |
| `pseudo` | `/share` | NFSv4 path clients mount |
| `path` | `share` | Exported directory, relative to the volume; created mode `1777` on first start. Never the volume root, which also holds `.nfs-state` |
| `readOnly` | `false` | Export read-only |
| `squash` | `root` | Map client root (`root`), nobody (`none`) or every user (`all`) to `nobody` |
| `gracePeriod` | `90` | Seconds after a start or failover during which clients reclaim state |

One export per block; deploy another block for another export. Each node must have
`pkgs.nfs-ganesha` in `environment.systemPackages`.

## 3. Mounting

```sh
mount -t nfs4 -o vers=4.1,hard <VIP>:/share /mnt/files
```

Use `hard` (the default): during a failover, I/O blocks and then resumes instead of failing. Only
NFSv4 over TCP is served. There is no NFSv3, no rpcbind, no NLM and no UDP.

**Security.** Exports use `sec=sys`, so the server trusts the uid a client sends, and every
client that can reach the VIP may mount the export. The VIP is a TCP splice that connects to
Ganesha from the VIP address itself, so Ganesha sees every client as the VIP and per-client
address rules cannot work. Keep the VIP on a network you trust, or filter clients in front of it.

## 4. Failover, from a client's perspective

When the node serving the export dies, the block, its volume's DRBD primary and its VIP move
together to a survivor. A `hard` mount does not fail during this time: I/O to the export blocks,
and the client keeps retrying the same VIP.

The new Ganesha reads the client records from `.nfs-state/recovery` on the volume and starts a
grace period (`gracePeriod`, default 90 s). Clients that were connected before the failure reclaim
their opens and locks. Grace ends as soon as every recorded client has reclaimed, or when the
period runs out, whichever comes first; until then new opens and locks wait. Files kept open
across the failure stay valid, so a process writing through one descriptor carries on.

Ganesha keys its client records by the address it sees. The VIP splice therefore connects to a
backend on the same node from the VIP address itself, which is the same on every node.
Otherwise the records written on the failed node would not match on the new one, and every
client would wait out the full grace period and lose its locks.

Measured in the `share-nfs` VM test (3 nodes, 5 s agent period, `gracePeriod: 30`): the block,
primary and VIP agreed on a survivor about 55 s after the serving VM was killed, and a writer
holding one descriptor open resumed about 76 s after the kill with every acknowledged line on the
volume. Most of that time is failure detection and rescheduling, not NFS.

## 5. What is not covered

- Kerberos (`sec=krb5*`), NFSv3, and per-client export rules.
- Quotas: size the volume instead.
- Clients other than the Linux kernel client are not exercised by the VM test.
