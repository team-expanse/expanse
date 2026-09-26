# Changelog

All notable changes to Expanse are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/).

History before 1.0.0 is not reconstructed here: `.plan/PHASE-01-TASKS.md`
through `.plan/PHASE-11-TASKS.md` are that record, phase by phase.

## 1.0.0 - 2026-09-26

First release. Everything listed under "What works today" in
[`README.md`](README.md) ships in this version, each with NixOS VM-test
coverage of real node kills, partitions and restarts.

### Added

- Installer ISO with TUI and unattended modes, declarative btrfs+LVM layout,
  impermanent root ([`docs/INSTALL.md`](docs/INSTALL.md)).
- Clustering: mDNS discovery, HMAC join tokens, a Raft control plane over
  mTLS, generations with rollback, cordon/drain/remove, witness nodes
  ([`docs/CLUSTERING.md`](docs/CLUSTERING.md)).
- DRBD-replicated volumes on LVM thin pools: create, resize, snapshot/restore,
  online repair, split-brain recovery ([`docs/STORAGE.md`](docs/STORAGE.md)).
- Blocks: a scheduler for systemd-supervised workloads with rolling updates,
  anti-affinity, singleton and daemonset placement
  ([`docs/BLOCKS.md`](docs/BLOCKS.md)).
- Postgres, iSCSI and QEMU/KVM blocks with failover
  ([`docs/DATABASE.md`](docs/DATABASE.md), [`docs/ISCSI.md`](docs/ISCSI.md),
  [`docs/VMS.md`](docs/VMS.md)).
- WireGuard mesh, per-node nftables, L4/L7 load balancing with VIP failover
  ([`docs/NETWORKING.md`](docs/NETWORKING.md)).
- Web UI with OIDC login ([`docs/WEB-UI.md`](docs/WEB-UI.md)).
- restic backup and restore of cluster config and volume data
  ([`docs/BACKUP.md`](docs/BACKUP.md)).
- Prometheus metrics and alert rules
  ([`docs/OBSERVABILITY.md`](docs/OBSERVABILITY.md)).
- Cluster CA with zero-downtime rotation; age-encrypted secrets
  ([`docs/SECURITY.md`](docs/SECURITY.md)).
- Rolling upgrades, one node at a time, with zero acked-write loss
  ([`docs/UPGRADE.md`](docs/UPGRADE.md)).

### Known issues

- Idle control-plane CPU with a replicated volume attached has not been
  measured on real hardware. Without volumes, bare metal measured 2.5% of one
  core on the Raft leader, within the 3% budget. With a volume attached, the
  leader is projected at about 4%. Details are under
  `node_control_plane_cpu_percent` in `test/perf/budgets.yaml` and in
  [`docs/HARDWARE.md`](docs/HARDWARE.md).
- SMB/NFS file shares are paused: Samba deploys, but failover has an open bug.
- The on-prem LLM subsystem is paused (no accelerator hardware to validate on).
- TPM sealing of secrets is deferred (no TPM hardware to validate on).
