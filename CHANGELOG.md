# Changelog

All notable changes to Expanse are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/).

History before 1.0.0 is not reconstructed here: `.plan/PHASE-01-TASKS.md`
through `.plan/PHASE-11-TASKS.md` are that record, phase by phase.

## Unreleased

### Changed

- The web UI has a proper app shell and design system: one shared layout
  with a sidebar, header (cluster name and quorum/health indicator, user
  menu, light/dark toggle), breadcrumbs and a footer naming the serving node
  and version; consistent status pills instead of raw enum constants;
  styled tables, cards, forms with inline validation, empty states, toast
  notifications for action results, and `<dialog>` confirmations for
  destructive actions. Responsive down to phone width and keyboard
  accessible. Hand-written CSS with tokens, an inline SVG icon sprite, no
  build step, no CDN; pages now send a self-only Content-Security-Policy.
- `/` is a live dashboard: nodes, quorum, blocks by phase, volumes by state,
  firing alerts, the current generation, a "needs attention" list and recent
  events.
- Every existing page (cluster, blocks, volumes, health, login) is restyled
  with no loss of function; SSE updates, CSRF and OIDC login are unchanged.

### Added

- Web UI pages for data the agent already served: **Nodes** (list with
  membership state and reported health; a detail page with inventory, health
  checks, reconciler status and resources for the serving node, clearly
  labelled as local to it), **Generations** (history, detail, diff of any
  two, rollback behind a confirm), **Events** (a live, type-filtered log of
  store writes with session keys redacted) and **Settings** (change the admin
  password, OIDC status, UI CA location and certificate download).
- `docs/WEB-UI.md` §5 describes every page.

### Fixed

- Plain form posts from a browser (deploy a block, create a volume) were
  always rejected with "CSRF token mismatch": the check read only the
  `X-CSRF-Token` header, which a form cannot send. Forms now carry the token
  in a `csrf_token` field, which the check also accepts.
- A node's reported health read "unknown" permanently: the agent's service
  PATH lacked `chronyc`, so the clock-sync check could never run. The agent
  now has chrony on its PATH whenever chrony is enabled.

## 1.1.4 - 2026-09-27

### Changed

- The mirror layout boots from either disk alone. The ESP and the system
  partition are now md RAID1 arrays across the first two disks (the ESP with
  metadata 1.0, so firmware reads each half as FAT), with btrfs on the md
  array; before, `/boot` lived only on the first disk and the btrfs RAID1
  root would not mount with a member missing. Verified in QEMU: install,
  boot with each disk removed, and a replacement disk rebuilt and booted
  alone. btrfs on md detects but no longer repairs system-volume corruption
  (`.plan/ARCHITECTURE.md` §3.4). Existing mirror installs keep the old
  layout until reinstalled; single-disk installs are unchanged. Also confirmed
  in a separate test run: a node with a removed drive still boots.
- On an md ESP, systemd-boot is installed with relaxed ESP checks and without
  an NVRAM entry; each disk boots through its `\EFI\BOOT\BOOTX64.EFI`.
- Mirror nodes ship `sgdisk` and log md events (a degraded array) to the
  journal. `docs/STORAGE.md` §4 has the disk-replacement runbook.
- Reinstalling the mirror layout over a previous mirror install formats
  fresh filesystems: a recreated md array exposes the old array's data, which
  disko would otherwise have kept (new `install-mirror` VM test).
- `expanse doctor storage` reads the md array under the system btrfs: PASS
  for a healthy md RAID1, WARN when degraded.

## 1.1.3 - 2026-09-27

### Fixed

An install from the ISO now works end to end: verified in QEMU through the
interactive installer, `nixos-install`, first boot, a one-node cluster running
a block, and a reboot that wipes root and keeps the cluster.

- Installer TUI: ENTER on the welcome screen did nothing (and other keys
  quit); a pasted SSH key was dropped; it ran as a service without the tools
  or `EXPANSE_FLAKE` it needs. It now starts from root's tty1 login, logs to
  `/tmp/expanse-install.log`, and warns when no SSH key is given. Static
  addressing moves to `expanse install --config`.
- The ISO's `<nixpkgs>` needed flakes, so disko failed.
- An installed node's configuration did not evaluate (`attribute 'disks'
  missing`): the disko layout is now applied through disko's own mapping.
- Installed nodes had no hardware configuration (the initrd could not find a
  virtio disk); the installer now runs `nixos-generate-config`.
- `/etc/nixos` was on the wiped root; it now lives in `/persist/etc/nixos`.
- The impermanence rollback raced the system disk and mounted it without
  `-t btrfs`, so root was never wiped on a real install.
- Installed nodes did not run the agent (`expanse.agent.enable` was only set
  by tests).
- `systemd-machine-id-commit` failed on every boot (the ID is persisted by its
  bind mount).
- `expanse cluster init` while `expansed` ran left the agent outside the new
  cluster; it now refuses and prints the stop/init/start steps.
- `expanse ctl block get` and `catalog get` crashed without `-o`.
- The web UI could not be opened in any browser (`SSL_ERROR_NO_CYPHER_OVERLAP`):
  it served the cluster CA's Ed25519 certificates, which browsers reject. It
  now has its own ECDSA P-256 CA, created once per cluster; import
  `/persist/expanse/ca/ui-ca.pem` (not `ca.pem`) into the browser. Node-to-node
  TLS and the metrics endpoint keep the cluster CA.
- An installed node's firewall blocked the web UI (8443) and the metrics
  endpoint (7447) from other machines; both are now open (TLS-authenticated).
- The ISO and installed nodes log to the serial console too (`ttyS0`), with a
  login there.

### Known issues

- Binary-backed block types (nginx, redis, ...) need their program on the
  node's `PATH`, which an installed node does not ship; `util/echo` runs.

## 1.1.2 - 2026-09-26

### Fixed

- `expanse version` on the installer and on installed nodes reported
  `0.0.1 (installer)`; both now report the release. The release version
  lives in `nix/version.nix`, and the ISO is named
  `expanse-<version>-<system>.iso`.

## 1.1.1 - 2026-09-26

### Changed

- DRBD resources set `c-min-rate 4M`: a new or rebuilt replica on a busy
  volume now syncs at 4 MiB/s or more instead of being throttled toward
  250 KiB/s. Existing volumes pick it up on upgrade through the agent's
  normal `drbdadm adjust`. This fixes the 1.1.0 known issue.

## 1.1.0 - 2026-09-26

### Added

- Single-node clusters: `cluster init --expect 1` forms a working cluster on
  one machine, and default volumes, blocks with storage, and VIPs run there
  ([`docs/CLUSTERING.md`](docs/CLUSTERING.md)).
- Volumes grow to their replication target automatically as nodes join, one
  fully synced replica at a time; DRBD quorum turns on live at three
  replicas ([`docs/STORAGE.md`](docs/STORAGE.md) §8).
- A new `UnderReplicated` volume state: every replica is healthy but there
  are fewer than the target. `volume list`, `volume inspect` and the web UI
  show "1 of 3 (no redundancy)".
- `volume list` shows volumes still waiting for placement, with the reason
  (for example `pending: needs 3 nodes, 1 eligible`).

### Changed

- A volume or block storage entry without an explicit replication takes its
  storage class's target and is placed on as many eligible nodes as exist, up
  to that target. It used to wait for three nodes. An explicit
  `--replication N` still waits for N nodes.
- `volume create --replication` defaults to unset (the class default)
  instead of 3; the web UI's create form does the same.

### Known issues

- A new replica's initial sync is throttled toward DRBD's `c-min-rate`
  (250 KiB/s) while the application writes heavily, so gaining a replica on
  a busy volume can be slow. This affects rebuilds in 1.0.0 too.
- The web UI's VIP takes one address from the external VIP pool; size the
  pool one larger than the block VIPs you need
  ([`docs/NETWORKING.md`](docs/NETWORKING.md)).
- Everything listed under 1.0.0 still applies.

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
