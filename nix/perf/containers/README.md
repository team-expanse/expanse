# X1 (real-hardware validation) via containers, not an install

Phase 11 Stream A (`.plan/PHASE-11-TASKS.md` D1/X1) needs to answer one specific open
question honestly, not a whole new hardware-compatibility sweep: is the 25–27% idle CPU
measured by `nix/tests/vol-constrained.nix` (against a ≤3% budget) a **VM-specific tax**
— `nanotime` falling back to a real syscall instead of vDSO under QEMU — or a real cost?
See `test/perf/budgets.yaml`'s `node_control_plane_cpu_percent` `known_gap` and
`ARCHITECTURE.md` §8/A45 for the full account.

This harness answers that question on real hardware without an actual install: 3
`systemd-nspawn` containers on this host (confirmed bare metal — `systemd-detect-virt`
reports `none`), each capped at 2 vCPU / 4 GB RAM, forming a real 3-node cluster with the
exact same `expanse` binary and measuring each agent's idle RSS and CPU with the same
helpers `nix/tests/vol-constrained.nix` uses (`nix/tests/python/node_overhead.py`). A
container shares the host kernel: no hypervisor, no emulated clock source,
`clock_gettime` runs through the same vDSO the host itself uses.

## Why no volumes, DRBD or disks

An earlier version also gave each container a real disk and ran vol-constrained's full
replicated-volume + hard-kill-failover scenario. It can't work: containers share one
kernel, and every piece of Expanse's data path is host-kernel-wide state. Device-mapper
and LVM names collided across nodes, and DRBD is a single host module whose resources
live in one global list looked up by name only (`drbd_find_resource()` in the 9.3.3
driver; only *connections* are network-namespace-aware). Expanse gives a volume the same
resource name and minor on every node (`internal/storage/drbd/alloc.go`), so three
replicas can't coexist in one kernel, and killing a container would leave its DRBD
resource alive anyway, so the failover wouldn't be real either. Replication and failover
are already proven in the VM tests; reconfirming them on these disks needs separate
kernels (KVM guests with the disks passed through), not containers.

So the agents here run with `storageVG` unset, which turns the whole volume stack off.

## What this does and does not prove

**Does:** whether expansed's idle CPU drops near budget without a hypervisor (confirms
the VM-tax theory) or stays high (rules it out, points back at real raft/lease/`bbolt`
fsync cost).

**Caveat:** vol-constrained measures with a replicated volume attached; this measures an
idle cluster with the volume stack off. A small gap between the two is expected; a
25%-vs-3% gap is not explained by that.

**Does not:** validate volumes, DRBD, failover, the ISO, disko partitioning,
impermanence's reboot-wipe, or firmware/NIC compatibility — `docs/HARDWARE.md`'s
"untested" rows stay untested.

## Design

- `host-containers.nix` — declares `boot.enableContainers = true`, an isolated bridge
  (`br-expanse`, no physical uplink), and `containers.n1/n2/n3`, each importing
  `self.nixosModules.expanse` unchanged plus container-specific overrides explained
  inline (inert `fileSystems` for `/`, `/nix`, `/persist`; classic dbus-daemon instead
  of dbus-broker). Impermanence's root wipe is initrd-stage and never fires in a
  container; its 4 regular bind mounts still need `/persist` sources, which `setup.sh`
  pre-creates.
- `container_adapter.py` — gives the nixosTest-driver-shaped calls
  (`m.succeed`/`execute`/`wait_for_unit`, `start_all()`, `subtest()`) a container
  backend via `nsenter`/`machinectl`.
- `nix/tests/python/idle_main.py` — the scenario: form the cluster, settle, measure
  every node's idle overhead, check the worst node against the two budgets. Shared
  unchanged with `nix/tests/node-idle.nix`, the VM twin, for a same-workload A/B.
- `run.py` — splices `container_adapter.py`, `cluster-common.py`, `vol_perf_lib.py`,
  `node_overhead.py`, the budgets and `idle_main.py`, and executes the result.
- `setup.sh` / `teardown.sh` — the privileged half (root required). Nothing they touch
  is on this host's own boot disk. All three scripts log full output to
  `/var/log/expanse-perf/`.

## Usage

```console
$ sudo ./setup.sh          # fresh /persist per node, rebuilds, (re)starts containers
$ sudo python3 ./run.py    # forms the cluster, measures idle RSS/CPU on every node
$ sudo ./teardown.sh        # tears everything back down
```

Record the result in `docs/HARDWARE.md` and `test/perf/budgets.yaml`'s `known_gap` note
once run — either resolved (real hardware lands under 3%, confirming the VM-tax theory)
or still genuinely over budget (a real product finding, not to be re-documented as a
known gap).
