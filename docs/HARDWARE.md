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

## Recording a result

Add a row per exact model with: ISO version, result (pass/fail), boot
time (`systemd-analyze`), install time, and any workarounds. Format:

```
| ThinkPad T430 (i5-3320M, 8 GB, 500 GB HDD) | ISO v0.1.0 | PASS | boot 38s | install 7m | n/a |
```
