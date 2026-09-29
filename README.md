# Expanse

A NixOS-based server infrastructure system: plug a machine in, join it to a
cluster, and get replicated storage, load-balanced services, and a control
plane that keeps running when any single node doesn't — without hand-rolled
distributed-systems code where a proven one already exists (Raft, DRBD,
LVM, WireGuard, nftables). See `.plan/PREMISE.md` for the full vision and
`.plan/ARCHITECTURE.md` for how it's actually built.

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
- **iSCSI** — an LIO target over a replicated volume with VIP failover.
  [`docs/ISCSI.md`](docs/ISCSI.md).
- **Virtualized workloads** — QEMU/KVM blocks with disks on replicated
  volumes, restarting elsewhere on node loss. [`docs/VMS.md`](docs/VMS.md).
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

**Paused, not built:** SMB/NFS file shares (Samba deploy works; failover
has an open bug — see `.plan/PHASE-03-TASKS.md`) and an on-prem LLM
subsystem (no accelerator hardware available to validate against yet —
see `.plan/PHASE-07-TASKS.md`). Both are independent of everything above
and can resume any time; nothing else depends on them.

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
| [`docs/ISCSI.md`](docs/ISCSI.md) | The iSCSI target block |
| [`docs/VMS.md`](docs/VMS.md) | The virtual machine block |
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

Expanse 1.0.0 (`.plan/ROADMAP.md`, all 11 phases shipped). Every feature listed
above under "What works today" has NixOS VM-test coverage exercising real
node kills, partitions, and restarts — not just unit tests. See
[`CHANGELOG.md`](CHANGELOG.md) for the release and its known issues, and
`.plan/ARCHITECTURE.md` §9 for the project's decision record.

## License

Expanse is licensed under the [Apache License, Version 2.0](LICENSE); see
[`NOTICE`](NOTICE). Contributions are accepted under the same licence.
Vendored Go modules keep their own licences.
