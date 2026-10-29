# Virtualized workloads

How to deploy a `vm/instance` block, what failover looks like from the guest's perspective, which
guest OS was actually validated, where guest disk/console state lives, and the live-migration scope
note. See `.plan/ARCHITECTURE.md` §9 (A33–A34) for the design rationale; this document is the
operator-facing companion.

## 1. The design, in one paragraph

`vm/instance` is a `SINGLETON` block, the same shape `share/smb`/`iscsi/target` already use: exactly
one instance, always co-located with its bound volume's DRBD primary (P12's colocation filter,
`ARCHITECTURE.md` A21). From the host's point of view the "workload" is just another
`RUNTIME_SYSTEMD` process — `cmd/expanse-block-run`'s `runVM` execs `qemu-kvm` directly (no
libvirt), the same "exec upstream tooling as a supervised foreground child" pattern `runSMB`/
`runISCSITarget` already use — so no new dispatch layer or runtime kind was needed (`proto.Runtime`'s
`RUNTIME_MICROVM` stays reserved and unused). The guest's virtual disk rides the same raw,
DRBD-replicated, never-formatted-by-the-host storage entry a LUN already uses (`filesystem: none`,
Phase 4 D3) — the guest owns and formats its own filesystem entirely itself. The guest's network
identity is a macvtap (mode bridge) device on the host's one shared interface, with a MAC pinned
once from the block instance's own name — no VIP, no proxy: ordinary switch MAC-learning plus the
guest's own gratuitous ARP on boot is enough for a client to reach the *same* address again after a
restart on a different node, unlike every prior phase's proxied-port shape.

## 2. Deploying a VM

```yaml
apiVersion: expanse.io/v1
kind: Block
metadata:
  name: vm1
  namespace: default
spec:
  type: vm/instance
  replicas: 1
  strategy:
    kind: SINGLETON
  resources:
    requests:
      cpu: 1000m
      memory: 2Gi
  storage:
    - name: disk
      size: 10Gi
      replication: 3
      mountPath: /mnt/vm1-disk
      filesystem: none        # the guest formats and owns this filesystem itself (D3)
  placement:
    requiredCapabilities: [kvm]
  config:
    bootKernel: "/path/to/kernel"
    bootInitrd: "/path/to/initrd"
    bootCmdline: "root=/dev/vda ..."
```

`placement.requiredCapabilities: [kvm]` restricts scheduling to nodes with real hardware
virtualization (`internal/agent/inventory`'s live-detected `kvm` capability, wired end-to-end for
the first time in Stream A — previously detected but never actually reaching the scheduler's
placement filter). `storage[0].filesystem` must be `none`: the block schema's raw-volume entry hands
`runVM` the bound volume's `/dev/drbdN` path directly as `-drive ...,if=virtio,format=raw`, never a
qcow2 or other nested image format, and the host never runs `mkfs`/`mount` against it. Apply it the
same way as any other block:

```sh
expanse ctl block apply -f vm1.yaml
```

### Sizing

`requests.cpu` becomes qemu's `-smp` (rounded up to whole vCPUs) and `requests.memory` its `-m`.
The scheduler places the block against what each node's agent publishes at
`/nodes/<id>/capacity`: hardware threads, total memory and the data volume group's size, with the
block's storage sizes counted against the volume group. CPU is overcommitted 2× by default; memory
never is. A node that has not published a capacity (an agent older than 1.10.0) counts as 4 CPUs,
8 GiB and 100 GiB.

Nothing is held back for the host by default, so on a node that also runs other blocks, reserve
some memory before placing a guest that takes most of it:

```sh
expanse ctl kv put /config/scheduler '{"reservedMemory": "64Gi"}'
```

Leave `limits.memory` unset on a VM: qemu needs memory beyond the guest's `-m`, and a limit equal
to the request gets the guest OOM-killed under load.

## 3. Guest OS installation and boot

### From a cloud image

The easiest way is the web UI: Blocks → Deploy block → `vm/instance`. Pick an image (Debian 13 or
Ubuntu 24.04 LTS, or Custom for your own URL), set CPUs, memory and the disk size, and paste your
SSH public keys. `gh:<user>` in the key list imports a GitHub user's keys when you press Deploy.
The same settings in a manifest:

```yaml
  config:
    image:
      url: https://cloud.debian.org/images/cloud/trixie/latest/debian-13-genericcloud-amd64.qcow2
      checksumUrl: https://cloud.debian.org/images/cloud/trixie/latest/SHA512SUMS
    cloudInit:
      sshKeys: ["ssh-ed25519 AAAA... me@laptop"]
      user: ops          # optional: a sudo user with the keys; unset, the keys go to the image's own user
      hostname: web1     # optional: defaults to the block's name
```

On the VM's first start, its node downloads the image (qcow2 or raw) into
`/var/lib/expanse/images`, checks it against `checksum` (SHA-256 or SHA-512 hex) or against the
image's line in the `checksumUrl` file, and writes it onto the volume. The block's status shows the
download's progress. The disk must be at least the image's size; the guest grows its root
filesystem to the rest. A qcow2 that names a backing or external data file is refused.

The image is written only while the volume's first MiB is still zero, and that MiB is written last.
A later start or a failover therefore boots the disk as it is, and a write cut short is redone on
the next start. Changing `image` later does not touch the disk.

Each start attaches a NoCloud seed (an ISO labelled `cidata`) with the user, keys and hostname.
cloud-init applies it on the first boot only, because the instance id stays the same; change keys
inside the guest after that. Use images with systemd 254 or newer (Debian 12 has 252), or set
`guestReady: none`; see below.

The guest joins the LAN through macvtap and gets its address from your DHCP server. The block's
page shows its MAC address, hostname and, once the guest agent answers, its address with a ready
`ssh` line (see below). The node running the VM cannot reach it; connect from another machine.
Downloaded images stay in the cache for other VMs; remove files there by hand to free the space.

### The guest's address

Every VM gets a virtio-serial port for the QEMU guest agent. Every 15 seconds, `expanse-block-run`
asks the agent for the addresses on the interface with the block's MAC address and adds them to
the unit's status text, e.g. `ready: multi-user.target reached (addresses: 192.168.1.50)`. The node's
Expanse agent copies it into the replica's status record (`/node/<node>/status/resources/block-replica:<ns>/<name>/0`),
and the block page shows it. Link-local addresses are left out.

The seed installs `qemu-guest-agent` and starts it on the first boot, which also runs the package
manager's update. Set `cloudInit.guestAgent: skip` to leave it out; the page then says no agent has
answered, and the router's lease list is the way to find the address. A VM deployed before this
change installs nothing new, as cloud-init runs once; install the package in the guest by hand.

### Running a command in the guest

The same agent runs commands in the guest as root, with no network or SSH key. As root, on the node
that runs the VM (its block page names the node):

```sh
expanse ctl vm exec vm1 -- systemctl status docker
expanse ctl vm exec -n apps vm1 -- docker exec pando-pando-1 pando admin setup-token
echo hello | expanse ctl vm exec -i vm1 -- tee /root/note
```

The output prints once the command exits, and its exit code becomes `vm exec`'s. Ctrl-C stops
waiting but leaves the command running in the guest. The agent's socket is
`/run/expblk-<ns>-<name>-0-qga.sock`, which only root can open; a VM started by an older Expanse
has it in the unit's private `/tmp` instead and needs a restart first.

### First-boot setup

`cloudInit.setup` installs packages, then runs shell commands as root, on the first boot only (a
failover boots the same disk and runs nothing again). In the web form, First-boot setup offers
ready-made setups; **Pando 0.12.0 under Docker** installs Docker and starts Pando from its release
compose file, checked against a pinned SHA-256. Give that VM 2 CPUs, 4 GiB and a 40 GiB disk or more.

```yaml
    cloudInit:
      sshKeys: ["ssh-ed25519 AAAA... me@laptop"]
      setup:
        packages: [nginx]
        runcmd:
          - systemctl enable --now nginx
```

The commands run as one shell script in order, and a failing one does not stop the rest; end a line
with `|| exit 1` to stop there. The VM counts as RUNNING before they finish. Their output is in the
VM's console log (`journalctl -u expanse-block-root@<namespace>-<name>-0` on its node) and in
`/var/log/cloud-init-output.log` in the guest. Like the rest of `cloudInit`, the setup sits in the
seed ISO on the node and is readable in the guest, so keep passwords out of it.

### Other ways

`bootKernel`/`bootInitrd`/`bootCmdline` boot a kernel directly (`-kernel`/`-initrd`/`-append`), and
a disk can be prepared by hand: write an image onto the volume's `/dev/drbdN` from the node that is
its DRBD primary before deploying the block, with `image` unset. [`PANDO.md`](PANDO.md) is a
worked, VM-tested example. Installing from an ISO with a web console is not built yet.

Tested guests: Debian 13 and Ubuntu 24.04 cloud images (checked in QEMU with the seed this
project builds), the NixOS cloud-init image `nix/tests/ui-vm-cloud-image.nix` deploys from the web
form, and the NixOS guests of the other `vm-instance*` tests. Other OS families (Windows, BSDs)
carry their own firmware and driver expectations that have not been measured.

### When a VM counts as RUNNING

A VM replica is RUNNING once its guest has booted, not merely once `qemu-kvm` has started. Each VM
gets a vsock device and the systemd credential `vmm.notify_socket`, so a guest running systemd 254
or newer reports its boot to `expanse-block-run` on its node. The guest counts as booted when it has
sent `READY=1` and `multi-user.target` is active. A guest in emergency or rescue mode also sends
`READY=1`, so it stays not ready. systemd 254 and 255 (Ubuntu 24.04) report no targets, so for them
`READY=1` alone counts, and emergency mode cannot be told apart. `nix/tests/vm-vsock-notify-probe.nix` measured these messages.

The guest's state is the unit's status text, so it can be read on the VM's node:

```sh
systemctl show -p StatusText --value expanse-block-root@default-vm1-0.service
# ready: multi-user.target reached
```

Other values are `booting`, `not ready: guest started, waiting for multi-user.target`, `not ready:
guest in emergency.target` (or `rescue.target`), `not ready: guest shutting down` and `not ready:
guest powered off (exit status N)`. A guest that never boots stays out of RUNNING; nothing restarts
it for that yet. The guest's serial console reads end-of-file, so an emergency shell gives up at
once and the guest carries on booting.

A guest without systemd never reports. Set `config.guestReady: none` for it, and the VM counts as
ready as soon as `qemu-kvm` starts, as it did before.

### Nested virtualization

By default the guest sees qemu's default CPU model, with no virtualization extensions, so it cannot
run KVM itself. Set `config.cpuModel: host` to pass the node's CPU through, including `vmx`/`svm`;
the guest then gets a working `/dev/kvm` and can run its own VMs, such as this project's VM tests.
The host's `kvm_intel` (or `kvm_amd`) module must allow nesting (`nested=Y`, the kernel default).

A host-CPU guest is tied to the CPU features of the node it boots on. It restarts cleanly on a node
with the same CPU, but on an older one it may see features disappear. Keep a cluster's nodes on the
same CPU generation if its VMs use `cpuModel: host`. `host` is the only value accepted; anything else
is refused at apply time. `nix/tests/vm-instance-host-cpu.nix` checks both models.

### Stopping a VM

A stop (a restart, a spec change, a move to another node) first asks the guest to power off, as
pressing its power button would, then waits up to `config.shutdownTimeout` seconds (0 to 80, default
60) before turning the VM off hard. A guest that ignores ACPI power events is always turned off hard
once the time runs out; set `shutdownTimeout: 0` for one to skip the wait.

### Suspending a VM

`expanse ctl vm suspend <name>`, or **Suspend** on the block's page, pauses the guest, writes its
memory to the node's disk under `/persist/expanse/vm-state/` and stops qemu. The block's phase reads
`SUSPENDED` once that's done. It keeps its place on the node and its share of the node's CPU and
memory, so a resume always fits.

`expanse ctl vm resume <name>`, or **Resume**, starts qemu from the saved state: processes, open
connections and tmpfs files are as they were. Clients see the pause as a long network stall, and
the guest's clock is behind until it next syncs. The save is deleted once it's loaded. A resume
sent before the phase reads `SUSPENDED` boots the guest fresh instead.

A suspended VM resumes from its save only on the node that wrote it. If it moves while suspended
(the node is drained or lost), it boots fresh on the new node, and the old node deletes its save, so
a guest never wakes up with memory that no longer matches its disk. A save that won't load, for
example after its config changed the VM's memory size, also ends in a fresh boot.

The save is about as large as the memory the guest was using, so leave room on the node's `/persist`
for it. Writing it may take a few minutes for a large guest; if it fails, the guest is powered off as
in a stop instead. Only `vm/instance` can be suspended (its `block.yaml` says `suspendable: true`).

### Machine type

qemu presents the guest a fixed set of virtual hardware, its machine type. `config.machine` pins it
(default `pc-i440fx-11.1`), so a newer qemu shows an existing guest the same hardware, and a VM's
saved state stays loadable. Only versioned `pc-i440fx-X.Y` and `pc-q35-X.Y` types are accepted.

## 4. Failover, from the guest's perspective

When the node running the VM is hard-killed, the disk's own DRBD promotion (already proven,
Phase 1–4) and the block scheduler's own `SINGLETON` rescheduling put a fresh `qemu-kvm` process on
the new primary's node, which cold-boots the guest — **not** a live migration; the guest's own
in-memory state at the moment of the kill is lost, same as pulling the power on a physical machine.
What survives:

- **The guest's network identity is unchanged** (X3) — the same pinned macvtap MAC comes back up on
  the new node, so a client reconnecting to the same address reaches the restarted guest with no
  manual reconfiguration; a stateful connection in progress at the moment of the kill still breaks
  (this is a cold reboot of the whole guest, not a seamless failover) and must be re-established the
  same way it would after any guest reboot.
- **The guest's own filesystem reports zero corruption** (X4) — verified under a real sustained,
  actively-in-flight fsync'd write load through the kill, not a static disk image
  (`nix/tests/vm-instance-fs-integrity.nix`): the new guest's own `e2fsck -fy` reports only
  journal-recovered errors, never uncorrectable ones, and every write the guest's own prior boot had
  fsynced before the kill is present with correct content.

`nix/tests/vm-instance-vertical-slice.nix` proves both together, against the *same* kill, staying
black-box about internal cluster state — it deploys, waits only until the guest's own write load is
confirmed actively flowing (which cannot happen at all unless DRBD promotion and guest boot already
succeeded), and kills whichever node currently holds the instance, rather than waiting on an internal
DRBD-primary-role check first the way the narrower per-stream tests do.

## 5. Where guest disk and console state lives

The guest's disk is the raw DRBD device backing the block's storage entry — inspect it directly from
whichever node currently holds the volume's DRBD primary role (`drbdadm status`, then whatever tools
the guest's own filesystem needs), the same "opaque, replicated, host-never-touches-it" shape a LUN
already has. The host process never mounts or formats it, by design (D3) — even once the guest itself
has formatted a real filesystem there, the host's own view of the device never changes.

Console access is the VMM's own serial channel, not a network hop: `runVM` execs `qemu-kvm` with
`-serial stdio`, so the guest's console (`ttyS0`) is the block's own systemd unit's stdout, captured
by journald under `expanse-block-root@default-<name>-0.service` like any other unit's output. This
was enough for every VM test in this project to observe guest-internal state (boot progress, write-
load status, fsck results) without ever needing the host to reach the guest over its own macvtap
network — which matters, since same-node host→guest IP reachability over macvtap has a known
limitation (`ARCHITECTURE.md` A34): a bridge-mode macvtap child is unreachable from its own lower
device's root netns. **Interactive console access** (attaching to that same stdio stream live, or a
VNC/SPICE web console integrated into the management UI) is documented as a known extension point,
not built this phase (D5) — the same "named so it isn't silently forgotten" treatment Phase 5's D6
gave backup hooks.

```sh
journalctl -u expanse-block-root@default-vm1-0.service -f
```

## 6. Live migration (X5, R4)

**Explicitly out of scope** — `ROADMAP.md` defers it. This phase's own restart mechanism is cold
migration: the guest's in-memory state is lost and it cold-boots fresh on the new node, exactly like
recovering from a power loss. This was a deliberate choice, not an oversight — cold migration reuses
every mechanism this project had already proven (DRBD promotion, `SINGLETON` rescheduling, macvtap
MAC-pinning) with zero new moving parts, at the cost of guest downtime spanning a full cold boot —
measured at 100.4s end-to-end in this project's own nested-KVM test budget (§7), inflated by nested
virtualization's own overhead relative to bare metal — rather than the sub-second pause a live
migration would give.

**What live migration would additionally need**, none of it built or started here:

- **In-flight memory-state transfer** — QEMU's own live-migration protocol (`migrate` over QMP)
  streams RAM pages from source to destination while the guest keeps running, converging to a final
  brief pause; this project's own QMP control-socket wiring (already used for `query-status`/`quit`)
  would need to drive that same protocol instead of a plain kill-and-restart.
- **Both VMM processes running simultaneously**, briefly — cold migration's scheduler action is
  "stop the old instance, start a new one somewhere else"; live migration needs the destination's
  `qemu-kvm` process already running and receiving the memory stream *before* the source stops,
  which the current `SINGLETON` strategy's placement model does not represent at all.
- **Storage that stays consistent without a promotion boundary** — cold migration relies on DRBD
  promotion only ever happening *after* the old holder is confirmed gone (the lease-gated mechanism
  every prior phase depends on); live migration needs the destination to have write-consistent access
  to the same disk *while* the source guest is still actively writing to it, a materially different
  (and riskier) storage-handoff shape than "promote once the old primary is provably dead."
- **A guest-quiesce/cutover handshake** — a clean cutover moment (freeze the guest, transfer the
  final memory delta, resume on the destination) with a defined failure mode if the handshake itself
  fails partway, unlike cold migration's simple "old process is just dead, no handshake exists."

This mirrors Phase 5's own X5 treatment of backup hooks: named explicitly as a real, valuable,
deliberately-deferred extension point, not silently forgotten and not partially built ahead of the
`ROADMAP.md` phase that actually owns it.

Separately, R4 asked whether cold migration's own guest-quiesce story — a hard kill mid-write to the
guest's own filesystem risking guest-level corruption, distinct from the already-proven host-level
raw-volume replication durability — needed any additional handling. **It did not**
(`nix/tests/vm-instance-fs-integrity.nix`): ordinary ext4 journal recovery on the guest's own next
cold boot was sufficient, with no additional guest-quiesce step needed before or during the kill.

## 7. Vertical-slice verification

`nix/tests/vm-instance-vertical-slice.nix` deploys a VM, waits only until its guest's own sustained
fsync'd write load is confirmed actively flowing, and kills whichever node currently holds the
instance — no internal DRBD-primary-role wait beforehand, unlike Stream B/C's own narrower tests.
Against that *same* kill it then confirms, in the same run: the guest is reachable again at its
unchanged macvtap identity (X3, with the reconnection latency measured directly, not just checked as
an eventual boolean), and the new guest's own `e2fsck -fy` plus a full write-by-write reconciliation
report zero corruption and zero missing acknowledged writes (X4). Measured, one representative run:
host-level re-convergence (block placement + DRBD primary agreeing on one survivor) took 56.8s; the
guest itself was reachable again at its unchanged address 100.4s after the kill (the larger of the
two numbers, since it additionally includes a full guest cold boot on top of host-level
re-convergence); the new guest's own verdict was `{'last': 21, 'ok': 21, 'bad': 0, 'fsck_rc': 1}` —
every one of 21 acknowledged writes intact, only journal-recovered (never uncorrectable) filesystem
errors. This is the closest exercise of X1/X3/X4 together this project runs for `vm/instance`, the
same role `db-postgres-vertical-slice.nix` and `iscsi-vertical-slice.nix` play for their own phases.
One design finding along the way: measuring X3's resume time requires its own dedicated polling loop
*after* host-level re-convergence is confirmed, not interleaved with it — host-level convergence and
guest boot are on different timescales, and a combined loop that exits as soon as the host-level
signal is satisfied never gives the still-booting guest a chance to answer a ping at all.
