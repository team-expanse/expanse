# X1 (real-hardware validation) via containers, not an install

Phase 11 Stream A (`.plan/PHASE-11-TASKS.md` D1/X1) needs to answer one specific open
question honestly, not a whole new hardware-compatibility sweep: is the 25–27% idle CPU
measured by `nix/tests/vol-constrained.nix` (against a ≤3% budget) a **VM-specific tax**
— `nanotime` falling back to a real syscall instead of vDSO under QEMU — or a real cost?
See `test/perf/budgets.yaml`'s `node_control_plane_cpu_percent` `known_gap` and
`ARCHITECTURE.md` §8/A45 for the full account.

This harness answers that question on real hardware without an actual install: 3
`systemd-nspawn` containers on this host (confirmed bare metal — `systemd-detect-virt`
reports `none`), each capped at 2 vCPU / 4 GB RAM, each with a real dedicated disk
(`/dev/sdb`/`sdc`/`sdd`) backing a real DRBD-replicated volume, running the exact same
`expanse` binary and the exact same measurement code
(`nix/tests/python/vol_constrained_main.py`) the VM test does — reused completely
unmodified, not reinvented. A container shares the host kernel: no nested hypervisor, no
emulated clock source, `clock_gettime` runs through the same vDSO the host itself uses.

## What this does and does not prove

**Does:** whether the CPU number drops near budget on real hardware (confirms the
VM-tax theory) or stays high (rules it out, points back at real raft/lease/`bbolt` fsync
cost) — the actual open question. Also exercises a real DRBD-replicated volume and a
real hard-kill failover on real disks, both already proven correct in VMs, so this
mainly reconfirms them under real I/O latency rather than answering a new question.

**Does not:** validate the ISO, disko partitioning, impermanence's reboot-wipe, or
firmware/NIC compatibility — `docs/HARDWARE.md`'s "untested" rows stay untested. Only an
actual install exercises those; nothing here touches this host's own boot disk.

## Design

- `host-containers.nix` — declares `boot.enableContainers = true`, an isolated bridge
  (`br-expanse`, no physical uplink), and `containers.n1/n2/n3`, each importing
  `self.nixosModules.expanse` **completely unchanged** — the same module every real
  install and every nixosTest VM node already uses — plus two narrow,
  container-specific overrides (both explained inline in the file):
  - `storage.nix`'s `neededForBoot = true` on `/`, `/nix`, `/persist` is forced back to
    `false`: those are real disko-declared filesystems on an install, but a container
    gets all three from nspawn's own external bind mounts instead, never declared in
    this module's own `fileSystems`.
  - `impermanence.nix`'s root-wipe mechanism is an **initrd-stage** unit; a container
    never runs an initrd, so it simply never fires — impermanence itself isn't being
    tested here. Its 4 *regular* (non-initrd) bind-mount `fileSystems` entries
    (`/etc/machine-id`, `/var/lib/nixos`, `/var/lib/systemd`, `/root/.ssh`, all sourced
    from `/persist/...`) still run at normal boot, though, so `setup.sh` pre-creates
    their sources once on the host side of the `/persist` bind mount before first start.
  - A small oneshot unit creates the `expanse` LVM VG + thin pool on the container's
    dedicated disk, mirroring `nix/tests/modules/storage-test.nix`'s own idempotent,
    never-wipes-an-existing-VG shape.
- `container_adapter.py` — the only genuinely new code. Gives `cluster-common.py` /
  `vol_cluster.py` / `vol_constrained_main.py`'s nixosTest-driver-shaped calls
  (`m.succeed`/`execute`/`wait_for_unit`, `start_all()`, `subtest()`) a real backend:
  `nixos-container run`/`machinectl` instead of a QEMU `Machine` object.
- `run.py` — splices `container_adapter.py` ahead of the exact same files
  `cluster-rolling-upgrade.nix`-style tests splice via `readFile`
  (`cluster-common.py`, `vol_cluster.py`, `vol_perf_lib.py`, `vol_constrained_main.py`),
  completely unmodified, and executes the result. Single source of truth: if those files
  change, this picks the change up automatically.
- `setup.sh` / `teardown.sh` — the privileged half (root required). Everything they
  touch is listed at the top of `setup.sh` and is reversible; nothing touches this
  host's own boot disk or root filesystem.

## Usage

```console
$ sudo ./setup.sh          # provisions disks, containers, rebuilds, starts them
$ sudo python3 ./run.py    # forms the cluster, creates a volume, measures, hard-kills
                            # the primary, times failover — prints RELEASE GATE PASSED
                            # or the specific budget(s) violated
$ sudo ./teardown.sh        # tears everything back down, wipes the 3 disks clean
```

Record the result in `docs/HARDWARE.md` and `test/perf/budgets.yaml`'s `known_gap` note
once run — either resolved (real hardware lands under 3%, confirming the VM-tax theory)
or still genuinely over budget (a real product finding, not to be re-documented as a
known gap).

## What's actually verified, and what still isn't

This session had no root access to run `setup.sh` itself, so it could not iterate on a
real boot the way the VM tests were iterated on. It could, and did, verify everything
short of that: `host-containers.nix` **evaluates and fully *builds*** under
`nix build` (not just `nix eval`) — a real, complete `containers.n1` system closure
(`activate`, `init`, `etc`, `systemd`, and every `expanse-*` unit including
`expansed.service`, `expanse-scratch-vg.service`, `expanse-firstboot.service`) was
produced and inspected on disk. That build pass, iterated exactly like a VM test run
(build, read the real error, fix the specific thing, rebuild), is what found and fixed
three real container-specific incompatibilities in the first place, each noted inline
in the file: `fileSystems.<path>.fsType` has no default and NixOS's own zfs-detection
forces its evaluation for every declared mount even under `boot.isContainer`; `/`
specifically hard-requires a `device` string with no default; and nixpkgs' own
`virtualisation/container-config.nix` forces `services.lvm.enable = false` by default
for every container, conflicting with `agent.nix`'s `true` (ours wins via `mkForce`,
deliberately, since DRBD/LVM management is the entire point of this container).

**Not** independently confirmed by a real boot, and the most likely places a first run
still surfaces something: whether `network-base.nix`'s WireGuard mesh device creation
works with a container's default capability set; whether DRBD's userspace tools
(`drbdadm`/`drbdsetup`) behave identically talking to the host's shared kernel module
from inside a container's mount/PID namespace as they do in a VM's own private kernel;
and whether the `/persist` pre-seeding `setup.sh` does is sufficient for
`impermanence.nix`'s 4 bind-mount `fileSystems` entries to actually mount cleanly at
runtime (their *shape* was confirmed valid by the build; whether their *sources* are
adequate can only be confirmed by a real boot). Treat the first `sudo ./setup.sh` run
the same way every VM test in this project was actually built: expect it to surface one
more real thing at the boot/runtime layer, read it, fix the specific thing, rerun — not
a sign the design is wrong. Report back what happens and I'll fix it from the real
output, the same way every fix above came from a real `nix build` error, not a guess.
