# Changelog

All notable changes to Expanse are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/).

History before 1.0.0 is not recorded here.

## Unreleased

### Fixed

- A two-replica volume no longer split-brains when its primary's node is
  cut off from the cluster. Previously the cut-off primary kept writing
  while the other side promoted, and on reconnect DRBD saw two diverged
  copies, so the volume needed manual recovery and the block that had
  moved lost its storage. Two-replica volumes now get a diskless DRBD
  tiebreaker on a spare node, which turns DRBD quorum on: the cut-off
  side stops writing and the majority takes over. Clusters of two nodes
  have no spare and behave as before. See `docs/STORAGE.md` §8 and, for
  mixed-version clusters, `docs/UPGRADE.md`.
- A postgres primary cut off by a network partition now rejoins as a
  replica once the network heals. Its volume could not be released while
  postgres still had it open, and the agent kept retrying that release
  forever, unmounting the filesystem each time, so postgres was never told
  to step down. The node now keeps the volume once its lease is free again.

### Added

- `expanse ctl volume inspect` shows a two-replica volume's tiebreaker
  node, or `none` when no spare node exists to hold one.

## 1.2.3 - 2026-10-03

### Fixed

- `expanse ctl node cordon` no longer evicts a node's replicas. A cordoned
  node was treated as unreachable, so 30 seconds later its replicas were
  marked lost and replaced elsewhere. Cordon now only stops new placements.
- `expanse ctl node drain` now moves the node's replicas, daemonsets
  included, on the next controller pass instead of relying on that
  eviction. `ctl node list` and the status page show the node as
  draining, and `uncordon` ends the drain. A drain refused for lack of
  another placeable node no longer leaves the node cordoned.
- A daemonset replica on a cordoned node that goes silent is now dropped,
  as on any other unreachable node, instead of being kept indefinitely.

## 1.2.2 - 2026-10-02

### Fixed

- `expanse cluster token create/list/revoke` and `cluster ca
  rotate/status/complete` now go through the running agent, so they work
  from any node without stopping `expansed`. With the agent stopped (right
  after `cluster init`) they still open the store directly.
- `expanse ctl node remove` and `cluster leave` work from any node: a
  follower forwards the raft membership change to the leader. Previously
  `remove` had to run on the leader and `cluster leave` only worked when
  the CLI happened to become leader.
- A node removing itself is now fully removed. Removal writes the
  revocation before leaving raft, and the leader finishes any removal
  that stopped partway.
- `cluster leave` now revokes the node's identity, the same as
  `ctl node remove` (it previously only deleted the node record).

### Added

- `expanse ctl node transfer-leadership [node-id]`. The "transfer
  leadership first" errors pointed to a command that did not exist.

## 1.2.1 - 2026-10-02

### Fixed

- `expanse ctl node cordon`, `uncordon`, `drain`, `list` and `remove` now go
  through the running agent, so they work from any node without stopping
  `expansed`. Previously they opened the store directly, which meant stopping
  the leader's agent and often failed with "this node is not the raft leader"
  once the resulting election moved leadership. `remove` still has to run on
  the leader, since it changes raft membership. The `--data-dir` and
  `--node-id` flags are gone from these commands.
- Removed the unreachable lifecycle `ctl node inspect <id>`, which was hidden by
  the inventory `ctl node inspect`. `ctl node list` shows the same fields.

## 1.2.0 - 2026-10-02

### Changed

- A VM is RUNNING only once its guest has booted. Previously a VM counted as
  RUNNING as soon as `qemu-kvm` started, even if the guest never booted. The
  guest's systemd now reports its boot over vsock, and the VM's unit status
  says whether the guest is booting, booted, in emergency mode or shutting
  down. A guest without systemd can opt out with `config.guestReady: none`.
  See `docs/VMS.md`.

- Block readiness probes now run. A block's `tcp` or `http` readiness probe
  was validated (a VIP port requires one) but never executed, so the VIP load
  balancer only noticed a dead replica when a connection to it failed. Each
  node now probes the replicas it hosts and publishes the result, and the
  load balancer and DNS stop sending traffic to a replica whose probe fails.
  A new replica, and so its block, is not `RUNNING` until its probe passes.
  `exec` probes are not run yet.

- Block liveness probes now run. A replica whose `tcp` or `http` liveness
  probe keeps failing is restarted on its node, with a growing pause between
  restarts; after five restarts that do not fix it, the node marks it
  failed. See `docs/BLOCKS.md`.

- A replica that its node marked failed now moves to another node. Its
  placement is marked `FAILED` and a replacement is scheduled anywhere but
  that node; a singleton with storage only moves to a node holding a replica
  of its disk. A replica that fails on a second node as well is stopped
  rather than moved again, so a broken workload does not cycle through the
  cluster: the block goes `FAILED`, or `DEGRADED` while other replicas still
  run, until a new version of the block is applied. A daemonset replica is
  marked `FAILED` but stays on its node. The agent's new
  `livenessMaxRestarts` option sets how many restarts a node tries first.

- `expanse ctl block get` now prints a status table by default, with each
  replica's node, readiness, restarts and last probe result, and why a
  replaced placement was stopped. The web UI's block page shows the same
  columns. Use `-o yaml` or `-o json` for the full record, which now
  includes each placement's `health` and `message`. `ctl block list` shows
  ready/desired replicas.

### Fixed

- A new block could stay `SCHEDULING` long after its replica was running,
  most often on a freshly formed cluster. The block bridge took each
  replica's status record for a stale replica and deleted it every 10
  seconds, so the controller saw the replica as running only when its pass
  happened to fall in the few seconds after the record was rewritten.

- A block's ready replica count was always 0, in the web UI and the API.
  It now counts replicas that are `RUNNING` and whose readiness probe is not
  failing.

- An `iscsi/target` block's VIP could fail to come up. Creating the target
  also opened a default portal on port 3260 on every address of the node,
  so the VIP could not listen on that port whenever the target started
  first. The default portal is now removed.

- One failing workload could stop its whole node from taking new placements.
  Each reconcile pass overwrote the node's health with the worst health of its
  workloads, so a single crash-looping replica made the node look unhealthy
  until the next health check, 10 seconds later. It also briefly hid a node's
  lost quorum. The node's status now reflects only the node's own health
  checks. The reconcile summary moved to `/nodes/<id>/reconcile`.
- That overwrite also hid a second bug: a node whose clock had not synced
  could not take placements, because placement required overall health to be
  `healthy`. That is the first minutes after boot, and every node of a
  cluster without an NTP source. A node now stops taking new placements only
  when a check that stops it running workloads is unhealthy: disk space,
  memory, the Nix store or the cluster store. Clock sync, load and reconcile
  problems still show in the node's health and alerts.

- A node cut off from the cluster could keep answering on a VIP after another
  node had taken it over. On losing its lease, the node noticed only at its next
  2 s check and then wrote the VIP's holder record before dropping the address;
  in a partition that write can hang until it times out. It now drops the
  address as soon as the lease is lost, and writes the record afterwards.

## 1.1.9 - 2026-09-30

### Fixed

- Killing the raft leader could restart VM and other workloads on healthy
  nodes. While the cluster elected a new leader, a node's read of its desired
  state failed as unavailable, and the reconciler took the failed read for an
  empty desired state and stopped everything it ran; a VM workload then cold
  booted again once the new leader was up. A failed read now skips the round,
  and a desired-state entry that fails to decode keeps its workload running.
- A leader election could still restart a VM on a healthy node, through its
  disk. Reading a volume's record during the election failed, the failure was
  reported as "not found", and the component that writes each node's desired
  state dropped the VM's disk mount and changed its spec. The storage controller
  had the same blind spot and could request a second volume under an existing
  name. Read failures now keep their real kind, and both components skip the
  round instead of acting on a partial view.
- Workloads and their disks piled onto the same few nodes. Every replicated
  disk went to the same nodes (the lowest node IDs), a VM has to run where its
  disk is, and the least-loaded score saw no node capacity so it never counted.
  Disks now go to the nodes holding the fewest replicas, the scheduler knows
  each node's capacity, and workloads placed together see each other's load,
  so VMs spread evenly across the cluster.

### Added

- A cluster test with VM workloads that serve HTTP from replicated disks,
  watched from outside the cluster, measuring forming, node-loss failover,
  rejoin and leader-loss failover; it runs at 6 nodes and at 12, each VM
  pinned to its own physical cores. At 6 nodes a node loss costs a workload
  about 105 seconds (node declared lost, then a nested-VM cold boot) with no
  acknowledged write lost, and a leader loss costs nothing.

## 1.1.8 - 2026-09-29

### Changed

- Installs are about 2.5 times faster: the install step takes about two
  minutes in QEMU, down from five, and downloads 18 MiB instead of 192 MiB.
  The installed node now builds the installer's own `expanse`, so it is copied
  off the ISO rather than compiled; the ISO carries a reference node for each
  disk layout and the tools that disko and the node-specific parts build with;
  and `nixos-install` copies NixOS's small local-only derivations off the ISO
  instead of rebuilding them. The ISO grows from 1.47 GB to about 1.65 GB.
- The tty1 host console drops its Node ID row when the ID is only the
  hostname again (a clustered node) or a bare UUID.

### Fixed

- Installed nodes reported their commit as `installer`; they now report the
  commit the ISO was built from.

## 1.1.7 - 2026-09-28

### Added

- Expanse is licensed under the Apache License, Version 2.0: `LICENSE` and
  `NOTICE` at the top of the source tree, and `meta.license` on the Nix
  package.
- The `expanse` package ships `LICENSE`, `NOTICE`, a `THIRD-PARTY.md` listing
  every vendored Go module and its version, and each module's own licence,
  under `share/licenses/expanse`. Installed nodes and the installer link them
  at `/run/current-system/sw/share/licenses/expanse`. Earlier ISOs shipped the
  binaries without the notices their MIT and BSD modules require.
- `scripts/release.sh` publishes a release: it builds and checks the ISO,
  stages `SHA256SUMS`, the changelog notes and a `release.json` manifest,
  pushes the tag, creates the GitHub release with the ISO attached and runs
  the website's `tools/sync-release` hook. See `docs/RELEASING.md`.

### Fixed

- Nodes read DEGRADED for about two minutes after each boot, long enough to
  fire `ExpanseNodeDegraded`: the clock-sync check judged chrony's last
  correction, which is the clock step chrony makes at boot, instead of how far
  the clock is off now (`System time` in `chronyc tracking`).

## 1.1.6 - 2026-09-28

### Changed

- The installer TUI has a proper console layout: a framed, centred page that
  fits the terminal (80x25 upwards, redrawn on resize), a "Step n of 6"
  indicator, a disk table with model, size, type and current contents, a
  reverse-video selection cursor, a red destructive list on review, a real
  progress view (current stage, a progress bar, elapsed time and a scrolling
  tail of the log instead of raw `set -x` output) and a done screen with the
  node's addresses and the next steps: the web UI URL, `expanse cluster init
  --expect 1` and where the admin password is logged. Colours are the VT's
  16, box drawing falls back to ASCII with `EXPANSE_TUI_ASCII=1`, and the
  keys and flow are unchanged (typing the last `L` of `INSTALL` still starts
  the install). Terminal handling moved to the shared `internal/tuikit`.

### Added

- `expanse console`: a read-only host information screen, like ESXi's DCUI,
  that every installed node shows on tty1 in place of a login
  (`expanse-console.service`, restarted on failure; `getty@tty1` is masked
  while tty2+ and the serial console keep their gettys). It shows the
  version, hostname and node ID, the addresses and web UI URL, the cluster
  name, role and quorum (or how to form a cluster), health, CPU, memory,
  disks, md mirror state (arrays named after their `/dev/md/<name>` links;
  a degraded `[U_]` array is called out in red) and uptime, refreshing every
  3 s and on resize. Health comes from the agent's 10 s heartbeat key and
  the failing checks' names from `GetHealth` at most once a minute, so the
  console never re-runs the checks (or the store's Raft write probe) on
  every refresh; an unreachable agent reads "not running", a slow one does
  not. `--once` prints one screen to stdout. The `node-console`
  VM test reads tty1 through `/dev/vcs1` and checks tty2 and serial logins.

### Fixed

- Every node reported UNHEALTHY (and fired `ExpanseNodeUnhealthy`) for the
  minute or so after each boot while chrony was still synchronising. For the
  first 10 minutes after boot an unsynchronised clock now reads "unknown"
  ("synchronising since boot"); after that it is unhealthy as before.
- The clock-sync check's large-offset warning (> 100 ms, degraded) could
  never fire: it failed to parse chronyc's "+0.25 seconds" and the leap-status
  line then overwrote it.

## 1.1.5 - 2026-09-27

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
- The web UI footer read "Expanse dev" on installed nodes: the build stamped
  the release only into the CLI's own variables. `expanse version` and the
  UI now read the one version the package build stamps.

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
