# Expanse

A NixOS-based server infrastructure system: plug a machine in, join it to a
cluster, and get replicated storage, load-balanced services, and a control
plane that keeps running when any single node doesn't — without hand-rolled
distributed-systems code where a proven one already exists (Raft, DRBD,
LVM, WireGuard, nftables). See `.plan/PREMISE.md` for the full vision and
`.plan/ARCHITECTURE.md` for how it's actually built.

The project site is [expanseos.org](https://expanseos.org).

## What works today

- **Install** — bootable ISO, TUI or unattended install, declarative
  btrfs+LVM partitioning, impermanent root (wiped every reboot except
  `/persist`). [`docs/INSTALL.md`](docs/INSTALL.md), [`docs/HARDWARE.md`](docs/HARDWARE.md).
- **Clustering** — mDNS discovery, HMAC join tokens, a Raft-replicated,
  mTLS-secured control plane, generations with rollback, cordon/drain/remove
  node lifecycle, witness nodes. Starts on a single node and grows as nodes
  join. [`docs/CLUSTERING.md`](docs/CLUSTERING.md).
- **Storage** — DRBD-replicated volumes on LVM thin pools: create, resize,
  snapshot/restore, online repair, split-brain recovery, losing a node for
  good. [`docs/STORAGE.md`](docs/STORAGE.md).
- **Blocks** (services) — a scheduler that deploys, scales, rolling-updates,
  and reschedules systemd-supervised workloads with anti-affinity,
  singleton, and daemonset placement. [`docs/BLOCKS.md`](docs/BLOCKS.md).
- **Database** — a Postgres block with streaming replication and
  automatic failover. [`docs/DATABASE.md`](docs/DATABASE.md).
- **MariaDB** — a MariaDB over a replicated volume behind a VIP; every
  committed row survives failover. [`docs/MARIADB.md`](docs/MARIADB.md).
- **iSCSI** — an LIO target over a replicated volume with VIP failover.
  [`docs/ISCSI.md`](docs/ISCSI.md).
- **NFS** — an NFSv4.1 export (NFS-Ganesha) over a replicated volume; clients
  reclaim their state on failover. [`docs/NFS.md`](docs/NFS.md).
- **S3 object storage** — Garage over a replicated volume behind a VIP;
  objects and keys survive failover. [`docs/S3.md`](docs/S3.md).
- **Virtualized workloads** — QEMU/KVM blocks with disks on replicated
  volumes, restarting elsewhere on node loss. [`docs/VMS.md`](docs/VMS.md).
- **Pando** — a guest image that runs the Pando app platform in a VM block;
  Pando and its apps survive failover. [`docs/PANDO.md`](docs/PANDO.md).
- **Networking** — a WireGuard mesh, per-node nftables, and L4/L7 load
  balancing with VIP failover. [`docs/NETWORKING.md`](docs/NETWORKING.md).
- **Web UI** — a browser-based cluster console (HTMX/SSE), OIDC login.
  [`docs/WEB-UI.md`](docs/WEB-UI.md).
- **Backup/restore** — restic-based backup of both cluster config and
  volume data. [`docs/BACKUP.md`](docs/BACKUP.md).
- **Observability** — Prometheus metrics, alert rules, node/volume/quorum
  health. [`docs/OBSERVABILITY.md`](docs/OBSERVABILITY.md).
- **Security** — a cluster CA with zero-downtime rotation, HMAC join
  tokens, mTLS everywhere, OIDC login for the web UI. Secrets are
  age-encrypted today; TPM sealing is a named, deliberately deferred
  future feature (no TPM hardware has been available to validate it).
  [`docs/SECURITY.md`](docs/SECURITY.md).
- **Rolling upgrades** — switch one node at a time with continuous
  availability and zero acked-write loss. [`docs/UPGRADE.md`](docs/UPGRADE.md).
- **Agent internals** — the desired-state reconcile loop every node runs.
  [`docs/AGENT.md`](docs/AGENT.md).

**Paused, not built:** SMB failover (Samba deploy works; failover has an open
bug) and an on-prem LLM subsystem (no accelerator hardware available to
validate against yet). Both are independent of everything above and can
resume any time; nothing else depends on them.

## Quickstart

```sh
# Build (or download) the installer ISO and write it to a USB stick.
nix build github:expanse/expanse#iso
sudo dd if=result/iso/*.iso of=/dev/sdX bs=4M status=progress conv=fsync

# Boot the target machine from it, then either:
expanse install --tui        # interactive, or
# ...or an unattended install — see docs/INSTALL.md for both.
```

Minimum target hardware: 2 CPU cores, 2 GB RAM, a 20 GB disk — a decade-old
laptop is the design point, not an afterthought. Full walkthrough,
including forming and joining a cluster afterward:
[`docs/INSTALL.md`](docs/INSTALL.md) → [`docs/CLUSTERING.md`](docs/CLUSTERING.md).

## Documentation

| Doc | Covers |
|---|---|
| [`docs/INSTALL.md`](docs/INSTALL.md) | Installer, disk layout, first boot |
| [`docs/HARDWARE.md`](docs/HARDWARE.md) | Supported/tested hardware, minimum spec |
| [`docs/CLUSTERING.md`](docs/CLUSTERING.md) | Raft substrate, join/discovery, CA, generations |
| [`docs/AGENT.md`](docs/AGENT.md) | The per-node desired-state reconcile loop |
| [`docs/STORAGE.md`](docs/STORAGE.md) | Replicated volumes, repair, split-brain recovery |
| [`docs/BLOCKS.md`](docs/BLOCKS.md) | Deploying and scaling services |
| [`docs/DATABASE.md`](docs/DATABASE.md) | The Postgres block |
| [`docs/MARIADB.md`](docs/MARIADB.md) | The MariaDB block |
| [`docs/ISCSI.md`](docs/ISCSI.md) | The iSCSI target block |
| [`docs/NFS.md`](docs/NFS.md) | The NFS export block |
| [`docs/S3.md`](docs/S3.md) | The S3 object storage block |
| [`docs/VMS.md`](docs/VMS.md) | The virtual machine block |
| [`docs/PANDO.md`](docs/PANDO.md) | Pando in a virtual machine block |
| [`docs/NETWORKING.md`](docs/NETWORKING.md) | Mesh, firewall, load balancing |
| [`docs/WEB-UI.md`](docs/WEB-UI.md) | The browser console |
| [`docs/BACKUP.md`](docs/BACKUP.md) | Backup and disaster recovery |
| [`docs/OBSERVABILITY.md`](docs/OBSERVABILITY.md) | Metrics, alerts, dashboards |
| [`docs/SECURITY.md`](docs/SECURITY.md) | CA, tokens, TPM sealing, mTLS |
| [`docs/UPGRADE.md`](docs/UPGRADE.md) | Rolling a fleet onto a new build |
| [`docs/DEVELOPING.md`](docs/DEVELOPING.md) | Dev environment, build, test, lint |

## Contributing / developing

```sh
nix develop      # go, golangci-lint, protoc, qemu, everything else needed
make build && make test && make lint
```

See [`docs/DEVELOPING.md`](docs/DEVELOPING.md) for the full workflow,
and `.plan/ARCHITECTURE.md` for the design principles (adopt proven data-plane
components; build the control plane) new code is held to.

## Status

All 11 phases of `.plan/ROADMAP.md` have shipped. Every feature listed
above under "What works today" has NixOS VM-test coverage exercising real
node kills, partitions, and restarts — not just unit tests. The latest
release and its installer ISO are on the
[releases page](https://github.com/team-expanse/expanse/releases/latest);
[`CHANGELOG.md`](CHANGELOG.md) records every release and its known issues, and
`.plan/ARCHITECTURE.md` §9 is the project's decision record.

## How Expanse is made

Expanse is a joint human–AI project. Its code, tests and documentation were
built by a human maintainer working with AI coding assistants. The AI wrote
much of the code and prose. The maintainer directs the work, reviews it and
decides what ships.

- **It is in the history.** Most commits credit an AI co-author in a
  `Co-Authored-By` trailer; `git log` shows which.
- **Claims come with evidence.** The VM-test coverage described under
  [Status](#status) exercises failures, not just the happy path, and
  [`CHANGELOG.md`](CHANGELOG.md) records what is unverified. Real hardware,
  for example, is still untested.
- **Judge it on its record.** Automated tests are no substitute for your own
  evaluation; read the known issues before trusting Expanse with data you care
  about.

See also [How Expanse is made](https://expanseos.org/#made-with-ai)
on the project site.

## License

Expanse is licensed under the [Apache License, Version 2.0](LICENSE); see
[`NOTICE`](NOTICE). Contributions are accepted under the same licence.
Vendored Go modules keep their own licences.
