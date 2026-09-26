# Hardware Matrix

Manual verification targets for each release. Failures become
known-issues, not release blockers — unless they affect the "old laptop"
class (that is a PREMISE commitment).

| Class | Example | Requirement | Status |
|---|---|---|---|
| Old laptop | ThinkPad T430, 8 GB, spinning disk | must work | untested |
| Mini PC | Intel NUC | must work | untested |
| Server | Dell R720 w/ HBA | must work | untested |
| ARM SBC | Raspberry Pi 4/5, 4 GB | should work (aarch64 ISO) | untested |
| VM | QEMU/KVM, Proxmox, VirtualBox | must work | QEMU/KVM: automated VM tests |

## Idle overhead on bare metal (Phase 11 X1)

Not an install: 3 `systemd-nspawn` nodes (2 cores / 4 GB each) on one bare-metal host,
volume-free idle cluster, versus the same scenario in VMs (`nix/tests/node-idle.nix`).
See `nix/perf/containers/README.md`.

| Host | Where | Leader CPU | Follower CPU | RSS | Budget |
|---|---|---|---|---|---|
| Xeon E5-2630 v3, bare metal | containers | 2.5% | 1.1–1.2% | 35–39 MiB | 3% / 200 MiB |
| same host | QEMU/KVM VMs | 16.4% | 7.4–7.5% | 29–97 MiB | 3% / 200 MiB |

## Recording a result

Add a row per exact model with: ISO version, result (pass/fail), boot
time (`systemd-analyze`), install time, and any workarounds. Format:

```
| ThinkPad T430 (i5-3320M, 8 GB, 500 GB HDD) | ISO v0.1.0 | PASS | boot 38s | install 7m | n/a |
```
