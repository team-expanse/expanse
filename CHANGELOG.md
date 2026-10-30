# Changelog

All notable changes to Expanse are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/).

History before 1.0.0 is not recorded here.

## 1.17.0 - 2026-10-30

### Added

- VM blocks can be suspended and resumed: `expanse ctl vm suspend|resume`, or Suspend and Resume
  on the block's page. A suspend saves the running guest's memory to its node's disk; a resume
  carries on where it left off, with its processes and connections. A VM moved while suspended
  boots fresh, and its old node deletes the save.
- A running VM grows and shrinks its vCPUs and memory with no reboot when a redeploy changes only
  `requests.cpu` or `requests.memory`, up to its `limits` (default twice the requests). The deploy
  form has the limit fields for VMs. The guest needs the `virtio_mem` driver and
  `memhp_default_state=online_movable` to give memory back (`docs/VMS.md`).
- When an upgrade brings a newer QEMU, a running VM's page shows **Update QEMU**, and
  `expanse ctl vm update` does the same: the guest is saved and resumed on the new QEMU without a
  reboot.
- A node that shuts down or reboots saves its running VMs instead of powering them off, and its
  next boot resumes them. The node announces the reboot, so the cluster keeps its replicas placed
  for up to 10 minutes rather than 30 seconds.
- `config.machine` pins a VM's QEMU machine type (default `pc-i440fx-11.1`), so a newer QEMU shows
  an existing guest the same hardware and its saves stay loadable.

### Changed

- Stopping a VM asks the guest to power off and waits up to `config.shutdownTimeout` seconds
  (default 60) before turning it off, instead of killing QEMU at once.
- A restart of the agent alone (a switch, `systemctl restart expansed`) leaves the node's
  workloads and volumes exactly as they are and moves nothing: the node stays healthy for up to
  2 minutes while the agent is away, and its volumes stay mounted and Primary. Before, a restart
  that took over 15 seconds let another node's copy of a busy volume be elected primary and later
  moved back. `systemctl stop expansed` behaves as before.
- Each running VM restarts once when its node moves to 1.17: its spec gains the placement id that
  guards its saves, and the 1.16 runtime can't take a spec change live. Later upgrades leave it
  running.
- The agent restarts when the node's block runtimes change, so it starts VMs on the new QEMU.
- On nodes with static addresses, the addresses stay up until the blocks and the agent have
  stopped at shutdown.

### Fixed

- An update that raises a block's CPU, memory or storage requests is refused when a node already
  running one of its replicas hasn't room for the increase. Replicas update in place (and a VM now
  resizes live), so the scheduler never checked it, and a node could be committed past its capacity.
- A node that comes back from a crash counts as up as soon as it reports in. Its agent used to
  report only 10 seconds after starting, and the leader cleared its "unreachable" mark up to
  5 seconds after that. A replica whose 30-second grace ran out in that gap was stopped and
  started again.
- Only the raft leader turns placements into each node's desired state, as designed. Every node
  did, so followers raced the leader with writes and deletes of their own. Each removal is now
  logged.
- `expanse ctl block apply` reports why an update of an existing block was refused. It used to
  fall back to creating the block and print only that it already existed.
- A VM whose guest crashes at boot is started again after a wait that doubles from 10 seconds up
  to 5 minutes, and its status says it crashed and when the next start is. qemu exits cleanly
  when a guest panics, so the agent used to start it again every 10 seconds, forever.
- A rolling update no longer counts retired placements as live, which could remove the running
  replica when a suspended VM that had moved was resumed.
- A db/postgres replica stopped partway through cloning from the primary no longer stays down.
  The clone carried the primary's own settings, so every later start used the primary's socket
  path and failed; a replica now clones into a staging directory and moves it into place once its
  own settings are written.
- `expanse ctl cluster leave` gets its answer: the agent waits for in-flight calls before exiting.

### Known issues

- storage/s3 (Garage 1.3.1) can lose the last object it acknowledged if its node crashes: Garage
  never syncs LMDB's metadata page. A patched Garage follows in 1.17.1.

## 1.16.2 - 2026-10-25

### Fixed

- A node that loses power soon after booting keeps its users. NixOS rewrites the user and group
  ID maps in /var/lib/nixos on every boot without syncing them; a power cut before the filesystem
  committed could leave them zeroed, and the next boot then created no users (sshd and chrony
  failed). Expanse now syncs them right after they are written.

## 1.16.1 - 2026-10-23

### Fixed

- util/restic-backup replicas start again: blocks that run as root (util/restic-backup,
  iscsi/target) never received their secrets, so restic stopped with "password is required" and
  an iSCSI target's CHAP password was missing.

## 1.16.0 - 2026-10-23

### Added

- A node's Hardware tab shows its management controller (iDRAC, iLO and the like): the address,
  with a link to its web interface, how it was assigned, and its MAC. The agent reads it through
  the local IPMI device, so no BMC credentials are needed; nodes without one show nothing.
  `expanse ctl node inspect` prints it too.
- ipmitool is installed on every node.
- An available upgrade shows across the web UI: **New** on the Upgrades rail item, a link in the
  footer, and **Update available** beside each node on an older release.

### Changed

- The DRBD kernel module moves from 9.2.16 to 9.2.18, which builds against Linux 6.18.57 with a
  small patch for that kernel's partial backport of `sockaddr_unsized`. 9.2.19 and 9.2.20 are
  skipped: their new equal-UUID reconcile can leave a replica stuck after a resync is requested.
- An upgrade's system build runs once more when a signal kills its builder, which VM tests have
  seen happen rarely; a second failure still stops the rollout.
- The Upgrades page picks the release from a list of the published releases instead of a text
  box. The daily check records the list, from GitHub's releases API.

### Fixed

- Test VMs and the installer ISO no longer fail to register their Nix store at boot under Nix
  2.34.9, which needs a home directory before `/etc/passwd` has root.
- The disk-space check lists each disk once, under its shortest mount point and fullest first,
  instead of every bind mount and btrfs subvolume of it; pstore and read-only images are left out.
- A new node advertises its default route's source address for raft and the API, never a
  link-local one: a Dell server's iDRAC USB NIC (169.254.0.2) came first and was picked.
  A node already advertising a link-local address moves to its default route's address on
  start, and once it leads, updates its raft membership entry and node record to match.

### Security

- nixpkgs moves to 2026-10-20 (02c087f) for upstream security fixes: Linux 6.18.57, systemd 262,
  OpenSSH 10.6p1, OpenSSL 3.5.9, QEMU 11.1.2, Go 1.26.9 and Chromium 155.0.8059.84.

## 1.15.0 - 2026-10-22

### Added

- A node's page has a Generations tab: its NixOS system generations, newest first, each with the
  Expanse release it runs. A generation that is exactly the system an upgrade built for its release
  is a release build; one that runs the release but was rebuilt since shows as modified, and the
  node header flags a running system that has drifted. Clean up keeps the newest N (3 by default)
  plus the running and booted generations, rewrites the boot menu without the rest and frees their
  space.

### Fixed

- Overview and Activity no longer fill with lease renewals and the reconciler's per-tick status
  rewrites on an idle cluster; a new lease holder or a status that changes still shows.

## 1.14.0 - 2026-10-22

### Changed

- The web UI has a new look and layout. A flat rail holds six sections: Overview, Cluster, Nodes,
  Workloads, Changes and Upgrades. Related pages sit behind tabs (Overview: Now, Checks, Activity;
  Workloads: Blocks, Volumes), and phones get a bottom bar with a More menu.
- Cluster has its own Settings tab for what every node shares: the LAN address range, the UI CA
  download and SSO status. The account menu's Settings keeps only the password. The UI CA is now at
  /cluster/ui-ca.pem and the LAN range form posts to /cluster/network.
- Node and block pages are tabbed. A node has Overview, Hardware and Checks; every block has
  Overview, Logs and Settings, and Delete moves to Settings.
- Deploying shows three marked steps (pick a type, configure and check, deployed), and the catalog
  groups types by category.
- Generations are now Changes: each generation's page shows what it added, removed and changed
  since its parent. Upgrades lists a running rollout node by node and keeps the start form visible
  but disabled while one runs.

### Security

- A db/postgres replica keeps its replication password's libpq passfile in a per-replica tmpfs
  directory (/run/expanse-block/<replica>), not on its volume, and a start deletes the copy that
  1.12.3 to 1.13.1 left there. The password no longer reaches disk in plain text.

## 1.13.1 - 2026-10-21

### Security

- A block's log lines are HTML-escaped before the web UI shows them. A block could log markup that
  the page acted on, such as a form posting with the signed-in operator's credentials.
- The deploy form's YAML view masks every secret field at any depth. A rejected deploy or a Check
  showed typed and generated passwords in it.
- Sign-in is bounded: at most two password checks run at once, a busy sign-in gets "try again"
  (429), and each failed attempt answers after a second, so a login flood cannot exhaust a node's
  memory. Changing the admin password shares the same limits.
- Signing out, or a session expiring, ends the live updates of pages still open in other tabs.

### Changed

- The web UI names nodes by hostname wherever it shows one, rather than the install UUID: lists,
  the node page, volumes, block replicas, deploy pre-flight, alerts, event keys and messages. Each
  node publishes its hostname; links still use the ID, and a node on an earlier release shows its ID
  until it is upgraded.
- Every page that shows changing state updates live, now including the blocks and volumes lists,
  the node page, generations and a block's address and guest network. Nothing reloads the whole
  page: a new volume's actions and a rollback's result arrive in place. Updates wait while a
  confirm dialog is open or a field has focus.

### Fixed

- The Health and alerts page no longer re-renders in a fast loop; its node checks keep their order.
- Live pages keep updating while the cluster has no leader, and recover from a failed read on
  their own rather than needing a reload.
- An expired session sends forms and live pages to the login page instead of failing silently.
- Single sign-on finishes signing in reliably and says why it failed (provider unreachable, expired
  request, the provider's refusal, or an email outside the allow-list), which ignores letter case.
- Forms submit once, keep what was typed when refused, and explain refusals: volume names and sizes,
  enabling storage, upgrade versions and the deploy form's ports.
- A block page no longer says "This block was deleted." when one of its replicas stops.
- Rollback is not offered when the current generation is unknown, Scale is offered only where it
  applies, the logs page lists only running replicas, and the Events page keeps 500 rows.
- A volume cannot be named `new`, which its page could never open.
- Smaller fixes: no "1970" probe times, long names wrap at phone width, a forced theme also themes
  form controls, and a click inside a dialog's padding no longer cancels it.

## 1.13.0 - 2026-10-20

### Added

- A VM's `cloudInit.setup` installs packages and runs commands on its first boot. The web form
  offers ready-made setups, starting with Pando 0.12.0 under Docker on a Debian 13 or Ubuntu 24.04
  image, so Pando deploys from the UI with no image to build.
- A VM's block page shows the guest's address and an `ssh` line to log in with. Each VM gets a QEMU
  guest agent port, the cloud-init seed installs `qemu-guest-agent` (`cloudInit.guestAgent: skip`
  leaves it out), and the node passes the addresses the agent reports to the page.
- `expanse ctl vm exec <block> -- <command>` runs a command in a VM's guest as root through its QEMU
  guest agent, from the node that runs the VM, and exits with the command's exit code.
- A VM that was running before the upgrade gets the guest agent port, its address on the page and
  `vm exec` after its next restart, since upgrades leave running blocks alone.

## 1.12.3 - 2026-10-20

### Changed

- The Pando guest (`docs/PANDO.md`) runs Pando 0.12.0, up from 0.10.0. Pando now flushes its secrets
  and API token keys to disk when it first creates them, creates its GitHub App itself, and connects
  Slack in one step. The compose file is unchanged. Existing guests are not upgraded in place.

### Fixed

- Upgrading a node no longer restarts its blocks that have more than one config field. Each
  agent build spaced a block's config JSON its own way, and the new agent took the difference
  for a config change. Configs are now encoded the same way by every build and compared by
  content, so an upgrade from 1.12.2 or earlier keeps them running too.
- The web UI keeps its TLS certificate across agent restarts and upgrades, so a browser that
  accepted it is not asked again. A node issued a new one at every start.
- Upgrades remove the release downloads (source tarball and binary cache, several GB) once the
  node runs the release, and remove an earlier release's downloads before fetching. Both the web
  UI's upgrades and `scripts/upgrade-node.sh` kept every download. The script keeps its log.
- A `db/postgres` block's replication password no longer reaches `postgresql.auto.conf` or the
  block's log. Standbys connect with a libpq passfile on their volume (0600) instead, and a standby
  cloned by an earlier release has its stored connection string rewritten at its next start.
  Earlier failovers logged the password in the block's journal, so rotate it if that matters.
- A node no longer runs `drbdadm adjust` on every volume pass, logging "rs_discard_granularity
  changed to 1048576" every 2 s. The volume config now asks for the resync discard granularity
  the kernel keeps for the backing LV (a thin pool's chunk size, 1 MiB on larger pools), so
  `adjust` finds nothing to change.

## 1.12.2 - 2026-10-20

### Fixed

- Upgrading from the web UI works: a node read every release's `nix/version.nix` with its leading
  comment, and refused the release as "the tarball holds release \"# The release version...\"".
  `nix/version.nix` is now a single line, so nodes on 1.12.0 and 1.12.1 can upgrade to this
  release from the Upgrades page; from this release on, comments there are skipped.

## 1.12.1 - 2026-10-20

### Added

- `scripts/upgrade-node.sh --boot` builds the release and makes it the boot default without
  switching, so a node's blocks keep running until it is rebooted; no drain is needed.
- `scripts/upgrade-node.sh` reuses a download from an earlier run when it still matches
  `SHA256SUMS`, instead of fetching the release cache again.

### Fixed

- `expanse upgrade` and `scripts/upgrade-node.sh` refuse a built system that does not run the
  release, put the previous source back and change nothing. A node whose `configuration.nix`
  fetches expanse itself, instead of importing `./expanse`, was "upgraded" to the new nixpkgs
  with the old expanse.

## 1.12.0 - 2026-10-19

### Added

- Upgrade the cluster from the web UI: Operations → Upgrades shows the newest release, each node's
  version and last upgrade, and the rollout. Upgrade cluster moves one node at a time, the leader
  last, and only while the cluster is healthy (every node in quorum, every volume Healthy or
  UnderReplicated); it halts on a node that fails or goes silent. Pause, Resume, Retry and Cancel
  change the rollout, and a node whose kernel, initrd or modules changed says "reboot to finish".
  See `docs/UPGRADE.md`.
- The leader checks GitHub for a new release once a day; the check can be turned off on the
  Upgrades page. `expansed --release-url/--release-key` point nodes at a mirror.
- `expanse upgrade start|apply|status`: download a release, check it, build and switch this node.
  Each release publishes its source tarball and a binary cache beside the ISO, and a `SHA256SUMS`
  signed with the Expanse release key (`expanse-release-1`). A node verifies the signature before
  it uses any of them, and trusts the release cache only with that key. A failed build puts the
  previous source back.
- `scripts/upgrade-node.sh` moves a node from 1.11 or earlier (no Upgrades page) to a release by
  hand, with the same checks; `docs/UPGRADE.md` "From 1.11 or earlier" walks a cluster through it.

### Changed

- An installed node builds with the nixpkgs its release's `flake.lock` pins (`nix/node-system.nix`),
  and its `NIX_PATH` names that same nixpkgs; the nixos channel no longer decides it. Every node
  built from one release builds the same system.
- Restarting or upgrading the agent leaves running blocks alone: block units are not restarted by a
  switch, `/run/expanse` survives an agent stop, and a volume a block holds open stays primary.
  No drain is needed to upgrade from 1.12 on. The move *to* 1.12 still restarts blocks, and asks
  for one reboot per node, since the DRBD module is rebuilt.

### Fixed

- After a `db/postgres` failover the surviving replicas follow the new primary again. Since 1.11.0
  the replication password is sealed, but failover still read it from the block's config, so the
  replicas retargeted with an empty password and stopped streaming.
- The DRBD module no longer depends on the `$SHELL` of whoever builds it, so a node's build matches
  the release cache instead of compiling DRBD and asking for a reboot.

## 1.11.3 - 2026-10-19

### Changed

- The Pando guest (`docs/PANDO.md`) runs Pando 0.10.0, up from 0.3.0. Pando now starts its own
  BuildKit, generates its Postgres password on the first start, and asks for a one-time setup token
  when no `PANDO_ADMIN_PASSWORD` is set. Existing guests are not upgraded in place.

### Fixed

- The Pando guest reports ready only once Pando answers and its key files are on disk. A crash
  soon after the first boot no longer leaves `token.key` empty, which kept Pando restarting forever.
- The Pando guest prints that Pando failed to start, instead of its console address, when it did.

## 1.11.2 - 2026-10-19

### Fixed

- A restarted agent takes back its own singleton lease at once, so a singleton that needs placing
  no longer waits out the old lease (up to its TTL) as "LeaseHeld".
- The installer fetches `gh:<user>` SSH keys from GitHub instead of writing `gh:<user>` into
  `authorized_keys`, where it let nobody in. It does so before touching any disk, and stops if the
  keys can't be fetched.
- `expanse ctl volume` commands take the volume ID that `volume list` shows first, as well as the
  name, and `volume list` keeps its columns lined up with long names.
- `expanse ctl block` commands take `namespace/name`, the form `block list` prints, as well as `-n`.
- The haproxy and caddy forms list the existing blocks' addresses under their frontends and sites.
- `expanse cluster status` shows every node's API address (the first node's was blank, joined
  nodes' had no host) and, when the cluster was formed by an older release, the version this node
  runs.
- `docs/INSTALL.md` §5 upgrades to the release you name and works on a node with no `expanse/` yet;
  `docs/STORAGE.md` says the DRBD 8.4 caveat only applies to nodes that had storage off.

## 1.11.1 - 2026-10-19

### Fixed

- A block's LAN address is reachable on a node with the stock firewall. The
  agent marks traffic for each block's address on its declared ports (nftables
  table `inet expanse-admit`), and the node's firewall accepts that mark; before,
  the port had to be opened by hand.
- The agent builds a block type's runtime on first placement again: its nix
  builds now go through nix-daemon. Before, they failed with `setting uid:
  Operation not permitted`, and the block stayed in Scheduling.
- A running singleton block no longer shows "Not fully placed: LeaseHeld" after
  its agent restarts.
- The deploy and edit forms show each address port (the port clients connect to
  on the block's address) as its own field. nginx's `port` setting is labelled as
  the port nginx listens on behind the address; setting it to 8081 used to leave
  clients on port 80 with nothing saying so.
- Changing a block's address port takes effect: the address is reopened on the
  new port. Before, it kept listening on the old one until the agent restarted.

## 1.11.0 - 2026-10-18

### Added

- Every catalog type ships a `template.yaml`: the strategy, ports, storage and
  resources a new block of that type starts from. `CatalogService.GetType` now
  returns the type's shipped `schema.json` as is, plus its defaults and
  template as JSON, so clients can build a deploy form from them.
- Deploy any catalog type from the web UI without writing YAML: Blocks →
  Deploy block lists the catalog (types no node can run are greyed out), and each
  type gets a form built from its schema, template and defaults. Errors show on
  the field they belong to; "Show as YAML" gives the same block as a manifest.
  A block's Edit button opens the same form.
- The LAN address range for block VIPs (and the UI's own) is a cluster setting,
  edited under Settings → LAN addresses in the web UI and checked against the
  nodes' subnet before it is saved. `expanse.agent.externalVIPPool` still
  works and applies while the setting is unset. Changing the range moves
  blocks to addresses in the new one.
- A block's page shows its address, and whether it is reachable from the LAN.
- Turn storage on from the web UI: a node's page has a Storage card with an
  Enable storage button, which builds the thin pool in the data VG and starts
  the node's volume stack without a rebuild or a reboot. The deploy form warns
  while no node has storage on. `expanse.agent.storageVG` still works and
  overrides the UI on that node.
- Block passwords and tokens are sealed: the fields a type's schema marks
  `writeOnly` are moved out of the block's config into the cluster's secret
  store (age, keyed from the cluster secret), whether they come from the form
  or `expanse ctl block apply`. Replicas get them as systemd credentials. They
  no longer appear in `block get`, the web UI, generation diffs or replica spec
  files. The deploy form generates a required password left empty and shows it
  once.
- The deploy form has a pre-flight: it shows the node each replica would run on,
  or why no node takes it, and warns when another block uses the same port.
  Check runs it, and admission, against the form's values without deploying.
- Deploy-form settings that point at another block offer the addresses of
  existing blocks of that type: grafana's `prometheusUrl` lists the
  `monitor/prometheus` blocks (and is filled in when there is one), redis's
  `replicaOf` the other redis blocks. Schemas mark such a field with
  `x-expanse-ref`.
- VMs from cloud images: `vm/instance` takes an `image` (URL plus a checksum
  or checksum file) that the node downloads, checks and writes onto the blank
  disk on the first start, and `cloudInit` settings (SSH keys, user, hostname)
  it hands to the guest as a NoCloud seed. The deploy form offers Debian 13,
  Ubuntu 24.04 LTS or a custom URL, imports `gh:<user>` SSH keys, and the
  block's page shows the guest's MAC address.

### Changed

- Every node loads the DRBD 9 kernel module and LVM thin-pool support from
  boot, whether or not storage is on, so turning it on needs no reboot.
- `spec.secrets` is accepted (V20): each entry names a sealed secret for one
  of the type's `writeOnly` fields. Blocks created earlier keep their
  passwords in their config until their next update, which seals them.
- VMs get a standard VGA device: Debian's cloud image resets right after
  GRUB without one.
- A VM whose systemd is 254 or 255 (Ubuntu 24.04) counts as RUNNING on
  `READY=1`; those versions do not report `multi-user.target`.
- Volume replicas are placed only on nodes whose storage is on. Nodes from
  before this release, which do not report it, stay eligible.
- New `db/postgres`, `db/redis` and `ai/ollama` blocks built from the catalog
  defaults listen on 15432, 16379 and 21434, so their VIP can take the standard
  client port (5432, 6379, 11434) on the same node, as `db/mariadb` already
  does. Existing blocks keep the ports in their config.

### Fixed

- A SINGLETON block without `replicas` (allowed, and how the deploy form
  writes prometheus, grafana, uptime-kuma, nfs, smb, s3, vaultwarden, caddy
  and vm/instance) was never placed. It is now stored with `replicas: 1`, and
  the scheduler counts one for a singleton either way.
- A `vm/instance` deployed from the form never got its disk: the type's
  template gave the volume no `mountPath`, and a volume without one is never
  bound to its replica.
- The worked example in `docs/BLOCKS.md` used lowercase enum values
  (`expose: vip`, `antiAffinity: node`, ...) that `expanse ctl block apply`
  rejects.
- The per-type docs no longer tell you to add the type's packages to
  `environment.systemPackages`: a node has fetched them itself since 1.10.1.
  `docs/OBSERVABILITY.md`'s Grafana example points at Prometheus's VIP on
  port 80, not 9090.

## 1.10.1 - 2026-10-17

### Fixed

- Installed nodes could not run any block type that execs an upstream
  program (every type except `util/echo`, `web/whoami` and
  `web/static-site`): nothing installed the programs, and the VM tests
  installed them by hand. `vm/instance`'s qemu (a headless build) now ships
  with every node; any other type's programs are fetched from the NixOS
  binary cache the first time a block of that type is placed on a node.
  See "Where a block's programs come from" in `docs/BLOCKS.md`.
- The btrfs system partition, which holds `/nix`, was a fixed 8 GiB, too
  small for a system upgrade plus the block programs now fetched on demand.
  The installer makes it 32 GiB when the system disks are 64 GiB or larger.
  Existing nodes can move `/nix` onto a logical volume: see "Moving /nix
  onto a logical volume" in `docs/INSTALL.md`.
- `nixos-rebuild` failed on every installed node: `configuration.nix`
  imported the installer's own copy of the source, which is gone after the
  first reboot. The installer now copies the source to
  `/etc/nixos/expanse` and imports it from there. See "Changing the node and
  upgrading it" in `docs/INSTALL.md`, which also covers fixing a 1.10.0 node.
- The node's DHCP client took a lease on each VM's macvtap link, which
  carries the guest's MAC, so the node claimed the guest's address and
  answered ARP and SSH for it. dhcpcd now ignores `mv*` links.
- Enabling storage with `nixos-rebuild switch` now loads `dm_thin_pool`, so
  the thin pool can be created straight away. Volumes need a reboot after
  enabling storage, because a switch loads the kernel's in-tree DRBD 8.4;
  until then `expansed` refuses to create volumes and says to reboot, where
  it used to write resources `drbdadm` could not parse.
- `restic` is installed on every node: `docs/BACKUP.md` backs up with it,
  and `expanse cluster restore` runs it.
- `expanse cluster status` showed `version: 0.1.0` on every cluster. It now
  shows `formed with:` and the release that formed the cluster.

## 1.10.0 - 2026-10-16

### Added

- `vm/instance` accepts `config.cpuModel: host`, which passes the node's
  CPU through to the guest, including `vmx`/`svm`, so the guest can run KVM
  itself. Left unset, guests keep qemu's default CPU model.
- A development guest for a `vm/instance` block: `nixosModules.dev-guest`,
  `packages.dev-guest-image` and `lib.devGuestImage`, which builds the image
  with your own keys and packages. It runs Expanse's VM tests under nested
  KVM. See `docs/DEVELOPING.md`.
- `expanse.devGuest.backup` backs up the dev guest's `/home` and
  `/etc/nixos` to any restic repository on a schedule, with `restic-dev`
  for restores.

### Fixed

- With `externalInterface` unset (the installed default), VIPs and VM
  macvtaps now use the default route's interface. Detection never matched a
  default route, so VIPs were never announced and VM blocks failed to start
  unless the interface was named explicitly. The WireGuard mesh's MTU check
  had the same fault.
- Placement now uses each node's real size: its agent publishes hardware
  threads, total memory and data volume group size. The scheduler used to
  count every node as 4 CPUs, 8 GiB and 100 GiB of disk, so no block asking
  for more could be placed anywhere. See "Sizing" in `docs/VMS.md`.

## 1.9.2 - 2026-10-14

### Fixed

- A VIP failover now has traffic flowing again within 15 s. The new holder
  took the VIP on time, but the load balancer still sent connections to the
  frozen node's backend and waited 5 s for each to time out. The L4 and L7
  load balancers now connect with a 2 s timeout and skip a backend for 10 s
  after a failed connect, falling back to every backend if none are left.
- A resource whose teardown failed is retried on the next reconcile instead
  of being forgotten. A volume unmount that raced a stopping database
  ("target is busy") left the volume mounted for good.

## 1.9.1 - 2026-10-12

### Security

- A removed node now leaves the WireGuard mesh and the firewall. Removal left
  the node's mesh key in the cluster store, so the remaining nodes kept it as a
  WireGuard peer and their firewalls still admitted its addresses. Removal now
  deletes the node's keys, and the mesh and firewall skip any revoked node
  whose keys remain from an earlier removal.

## 1.9.0 - 2026-10-09

### Added

- `media/immich` block: Immich, a photo and video library, on a replicated
  volume behind a VIP. The block runs Immich's own Postgres (with VectorChord)
  and Redis on the volume, with Redis syncing every queued job, so uploads and
  their pending jobs survive a crash of the serving node. See `docs/IMMICH.md`.

### Changed

- Expanse now follows nixos-unstable instead of nixos-26.05, whose Immich 2.7.5
  is end of life with known CVEs. Nodes keep DRBD's 9.2.16 kernel module:
  unstable's 9.3.3 does not support the 6.18 kernel.
- Grafana 13.2 no longer includes its Prometheus datasource. Nodes that may
  run `monitor/grafana` must also ship `packages.grafana-plugins`; the block
  links its plugins into Grafana on every start.

### Fixed

- A volume's primary no longer steps down when a lease renewal is slow. A
  renewal re-sent after a lost response failed against its own earlier commit,
  so a few hundred milliseconds of network delay could demote a healthy
  primary and return I/O errors to the applications using the volume.
- When a volume's lease is lost, the node now logs why (`volume lease lost`).
- A new thin volume's first sync no longer writes every block to its
  replicas. It copied the empty volume in full, filling each secondary's
  thin LV to 100% and taking minutes for a large volume (10 GiB in over
  4.5 minutes in the VM tests). Blocks that are all zeros now reach the
  replicas as discards, so they stay unallocated, and the same 10 GiB syncs
  in under 2 minutes. Rebuilding a replica of a mostly empty thin volume gains
  the same way. Thick volumes are unchanged.

## 1.8.0 - 2026-10-09

### Added

- `security/vaultwarden` block: Vaultwarden, a Bitwarden-compatible password
  manager, on a replicated volume behind a VIP. The block makes SQLite sync
  every commit, so items saved just before a crash of the serving node survive
  it. See `docs/VAULTWARDEN.md`.
- `media/jellyfin` block: Jellyfin, a media server, on a replicated volume
  behind a VIP, streaming media mounted at the same path on every node. The
  block makes SQLite sync every commit and keeps transcodes off the volume.
  See `docs/JELLYFIN.md`.
- `monitor/uptime-kuma` block: Uptime Kuma, status monitoring, on a replicated
  volume behind a VIP. Uptime Kuma has no setting for SQLite's sync level, so
  the block loads a preload that makes it sync every commit. See
  `docs/UPTIME-KUMA.md`.

## 1.7.0 - 2026-10-08

### Added

- `util/restic-backup` block: scheduled restic backups of block volumes to S3
  or any restic repository, with retention. Each volume is backed up from a
  crash-consistent snapshot on the node holding its primary, and the backups
  follow it through failover. See `docs/RESTIC-BACKUP.md`.

## 1.6.0 - 2026-10-08

### Added

- `dev/forgejo` block: Forgejo as a Git forge on a replicated volume behind a
  VIP serving HTTP and SSH. Its repositories, SQLite database, SSH host key and
  secrets survive a crash of the serving node, including a push acknowledged
  just before it. See `docs/FORGEJO.md`.

## 1.5.0 - 2026-10-08

### Added

- `web/caddy` block: Caddy as a web server and reverse proxy with automatic
  HTTPS, on a replicated volume behind a VIP serving 80 and 443. Its
  certificates, ACME account and internal CA survive a crash of the serving
  node. See `docs/CADDY.md`.
- `net/haproxy` block: HAProxy as a TCP and HTTP load balancer, run as
  stateless replicas behind a VIP, with health-checked servers, a stats page
  and Prometheus metrics. See `docs/HAPROXY.md`.

### Changed

- A block's VIP now serves every `EXPOSE_VIP` port, each forwarding to its own
  `targetPort`. Before, only the first exposed port was served.

### Fixed

- Deleting a block left its replicas' status records in the store. The
  controller promotes replicas from these records, so a block recreated with
  the same name could be marked running from the old ones. Each node now
  removes records whose resource it no longer runs, including ones already
  left behind.

## 1.4.0 - 2026-10-07

### Added

- `storage/s3` block: an S3-compatible object store on Garage, with its
  metadata, objects and keys on a replicated volume behind a VIP. It
  creates the configured access key and buckets, and every acknowledged
  object survives a crash of the serving node. See `docs/S3.md`.
- `db/mariadb` block: one MariaDB server on a replicated volume behind a
  VIP. Its root password, database and application account are set from
  the block's config on every start, and every committed transaction
  survives a crash of the serving node. See `docs/MARIADB.md`.
- Pando guest image: `nix build .#pando-guest-image` builds a NixOS disk
  image running Pando, its Postgres and BuildKit under Docker, to run in
  a `vm/instance` block, and `nixosModules.pando-guest` builds a
  customized one. Pando and the apps it hosts come back on a survivor
  when the node running the guest fails. See `docs/PANDO.md`.

## 1.3.0 - 2026-10-07

### Added

- `share/nfs` block: one NFSv4.1/4.2 export of a replicated volume,
  served by NFS-Ganesha behind an external VIP. When the serving node
  fails, the export moves with the volume and clients reclaim their opens
  and locks during the grace period, so a writer with a file held open
  carries on without remounting. See `docs/NFS.md`.
- `monitor/prometheus` block: scrapes every node's agent over the
  cluster's mutual TLS and the `monitor/node-exporter` daemonset, loads
  the shipped alert rules and keeps its data on a volume.
- `monitor/grafana` block: comes with the Expanse Prometheus datasource
  and the shipped dashboard already set up.

### Changed

- A VIP connection to a backend on the same node now comes from the VIP
  instead of the node's own address, so the backend sees one source
  address for a client before and after a failover.

### Fixed

- A node removed from the cluster could get back in by resuming an
  earlier TLS session, because a resumed session skipped the membership
  check. Every handshake, resumed or not, is now checked.
- The cluster store no longer panics when a slow watcher is dropped
  partway through delivering a change.
- `expanse` commands that run without an agent no longer fail when
  `TMPDIR` is a long path, which made the socket path too long.

## 1.2.6 - 2026-10-05

### Fixed

- A replica of a block with storage is no longer replaced on another node
  unless some other node holds a healthy copy of its volume. The check
  always passed before, so a replica on a failed node could be restarted
  elsewhere with no data, for example a postgres replica, whose volumes
  are not replicated. Such a replica now waits, keeping its place, and
  starts again when its node comes back; it also stays on a draining node
  for the same reason. The block reports "waiting for another node to
  hold a healthy copy of the volumes".
- A block that lists `spec.secrets` is now refused with "secrets are not
  supported yet". It used to be accepted, but nothing stored or delivered
  the secrets, so the workload started without them. Remove the entries
  to apply the block.

## 1.2.5 - 2026-10-04

### Fixed

- `expanse ctl volume snapshot` now waits until the primary has taken the
  snapshot and names the node that holds it. It used to return as soon as
  the request was queued, so a script could read or back up a snapshot
  that did not exist yet, and a refused request went unnoticed. It reports
  a refusal, and a request still queued after `--wait-timeout` (2m).
  `--no-wait` keeps the old behaviour.
- A node removed from the cluster no longer keeps running its block
  replicas. Its agent carried on with no quorum and treated the removal
  like a network partition, so the replicas ran twice, once on the
  removed node and once where the cluster replaced them. The removed
  node now stops its workloads and its agent exits for good, logging who
  removed it and why. A node removed while it was down or cut off learns
  of it from a peer once it is back. See `docs/CLUSTERING.md`.

## 1.2.4 - 2026-10-03

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
  replica once the network heals. While postgres still had its volume
  open, the agent detached the filesystem from under it and then retried
  the release forever, so postgres never saw the role change telling it to
  step down. A busy volume now stays mounted, and the node takes it back
  once its lease is free again.

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
