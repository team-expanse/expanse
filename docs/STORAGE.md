# Storage

How a node's disks turn into replicated volumes: the stack, the thin-pool policy, capacity
monitoring, and manual recovery from split-brain. See `.plan/ARCHITECTURE.md` §3 for the design
rationale; this document is the operator-facing companion.

## 1. The stack

```
GPT ─┬─ ESP (FAT32)             → /boot
     ├─ btrfs partition          → subvolumes @root @nix @persist @log
     └─ LVM PV → VG "expanse"    → thin pool → thin LVs
                                     └─ DRBD 9 (protocol C, quorum majority)
                                          └─ filesystem or raw (block replica)
```

The installer (`docs/INSTALL.md`) lays down the btrfs system partition and creates VG `expanse`
from the remainder of the disk(s); on a multi-disk server the system partition is btrfs RAID1
across the first two disks and every disk's remainder — including theirs — joins the VG as a plain
PV (JBOD; see §4). Nothing above the VG is created at install time: the thin pool and DRBD wiring
are an explicit opt-in, because they need `--storage-vg`/`--storage-pool` set and a kernel module
DRBD alone requires.

Two independent, in-tree filesystems, one out-of-tree module (DRBD). Never ZFS: it would have
added a second out-of-tree module gating every kernel upgrade, in a product whose central pillar
is deterministic, reproducible upgrades.

## 2. Enabling volume replication on a node

A freshly installed node has the VG but nothing built on it. Add to the node's NixOS config
(`expanse.agent`, `nix/modules/agent.nix`):

```nix
expanse.agent.storageVG = "expanse";
expanse.agent.storagePool = "pool";   # empty = thick volumes, no snapshots
```

then rebuild. This wires `boot.extraModulePackages`/`boot.kernelModules` for the DRBD kernel
module, enables `services.lvm.boot.thin.enable`, and passes `--storage-vg`/`--storage-pool`
through to `expansed`. It does **not** create the thin pool itself — that is a one-time,
per-node step because its size is a capacity decision, not something to infer:

```sh
lvcreate --type thin-pool -l 95%FREE -n pool expanse   # leave headroom; see §3
```

`expansed` then creates, resizes and deletes DRBD-backed thin LVs from that pool as
`expanse ctl volume` requests arrive; nothing about volume creation is manual after this point.

## 3. Thin-pool policy

The exhaustion mode must never fire by accident — an exhausted pool queues every write against it
until space is freed, which for a DRBD backing device means every replica on that node stalls.

- **No overprovisioning by default.** Keep the sum of volume sizes within the pool's size; opt in
  explicitly (`lvcreate -V` sizes exceeding pool capacity) only with a plan for the alternative.
- **Reserve headroom for snapshots.** Even with zero volume overprovisioning, a long-lived
  diverging snapshot can fill the pool on its own.
- **Monitor `Data%` and `Meta%` — separately.** Metadata fills independently of data and is easy
  to forget; `expanse doctor storage` (§5) checks both.
- **Verify discards reach the pool**, or it only ever grows. DRBD must pass discards down to the
  thin LV for space freed inside a volume to be reclaimed at the pool level.
- **Extend before you're forced to:** `vgextend expanse /dev/sdX` (more PVs) then
  `lvextend -l +100%FREE expanse/pool` (grow the pool) — both are online.

Thick LVs (`storagePool = ""`) remain a supported fallback if thin's first-touch allocation cost
proves material on the oldest target hardware; the trade is losing block-level snapshots for
opaque volumes.

## 4. Local redundancy

**Mirror the system volume on servers (btrfs RAID1); do not add local RAID for the data VG by
default.** Volumes are already replicated three ways across nodes by DRBD — local RAID under the
data PV buys MTBF, not correctness, since a node's pool being lost and resyncing from peers is a
case that has to work regardless. Prefer JBOD/HBA passthrough so btrfs owns the system mirror:
hardware RAID hides the second copy from btrfs, leaving it able to *detect* corruption but not
*repair* it. Never use btrfs RAID5/6 — it has an unfixed write hole. `expanse doctor storage`
flags both a single-device system volume (WARN: detected, not self-healing) and RAID5/6 (FAIL: a
hand-edited layout, since no installer layout ever offers it).

## 5. `expanse doctor storage`

```sh
expanse doctor storage --vg expanse --system-mount /
```

Five checks, PASS/WARN/FAIL, each with a remediation hint on anything short of PASS:

| Check | What it looks at | WARN | FAIL |
|---|---|---|---|
| `drbd-module` | `/sys/module/drbd/version` | loaded, version unknown | not loaded, or not 9.x |
| `volume-group` | the `--vg` VG's free space | ≤10% free | VG not found |
| `thin-pools` | every thin pool's `Data%`/`Meta%` | ≥80% | ≥90% |
| `system-mirror` | `btrfs filesystem show`/`df` on `--system-mount` | single device | RAID5/6 in use |
| `drbd-resources` | every configured resource's disk state, quorum, peer connection | none configured | no quorum, degraded disk, or a standalone (disconnected/split-brained) peer |

`--vg` empty skips the volume-group and thin-pool checks (degraded to WARN, not FAIL) — the
expected state on a node with volume storage not enabled at all. The command exits nonzero if any
row is FAIL, so it drops straight into a health-check script or systemd `ExecStartPre`.

## 6. Split-brain: manual recovery

DRBD's quorum majority and the Raft-backed volume lease together keep two nodes from writing to
the same volume at once — split-brain here means a *former* primary that kept accepting writes
after losing quorum (which cluster substrate is designed to prevent) or a resync interrupted by
another failure, not routine divergence. **It is never resolved automatically**: DRBD drops the
disagreeing replica's connection and marks it, and a human decides which side's data survives.

1. **See what's diverged:**

   ```sh
   expanse ctl volume diverged
   ```

   Lists every volume the store has moved to `NEEDS_MANUAL_RECOVERY`. `expanse doctor storage`
   also flags this, per-resource, as a standalone peer under `drbd-resources`.

2. **Decide which node's data to keep.** There is no automatic right answer — check
   `expanse ctl volume inspect <name>` for each replica's last-seen time and any application-level
   signal (e.g. which side has the newer writes a client actually confirmed).

3. **Resolve, keeping one side:**

   ```sh
   expanse ctl volume diverged <name> --choose <node>
   ```

   This **discards every change** made on the other replicas since the split, then reconnects them
   to resync from `<node>` and puts the volume back in service. There is no partial or merged
   recovery — DRBD replication is block-level, not content-aware.

4. **Confirm:** `expanse ctl volume inspect <name>` until every replica reports `HEALTHY`, or
   `expanse doctor storage` no longer flags the resource.

A **routine** out-of-sync replica (a resync interrupted by a reboot, not a split-brain) recovers on
its own — the node rejoins and DRBD resyncs it in the background. `expanse ctl volume verify` /
`resync` are for a different case: a scrub that finds silent corruption on an otherwise-connected
replica, not for split-brain recovery.

## 7. Losing a node for good

`expanse ctl volume retire <name> --node <node>` gives up a dead node's replica immediately,
without waiting for `--storage-lost-after` (default 10m) to elapse — for a node that is down and
not coming back. The controller refuses a node that is still alive, and a volume with no other
reachable replica; a spare node takes over the freed replica slot if one exists.
