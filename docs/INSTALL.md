# Installing Expanse

This guide takes a blank x86_64 or aarch64 machine to a running Expanse
node. It assumes no familiarity with Nix or btrfs.

## What you need

- Any machine with at least **2 CPU cores, 2 GB RAM, and a 20 GB disk**
  (a 10-year-old laptop is fine — that is the design target).
- A USB stick of at least 4 GB for the installer (the ISO is ~1.4 GB).
- An SSH public key (optional but recommended).

**Warning: installing Expanse destroys all data on the disks you select.**
After installation the root filesystem is wiped on *every reboot*; only
`/persist` survives. That is the point, but back things up first.

## 1. Make the installer USB

```sh
nix build github:expanse/expanse#iso
# or download the release ISO

# replace /dev/sdX with your USB stick (check with lsblk!)
sudo dd if=result/iso/*.iso of=/dev/sdX bs=4M status=progress oflag=direct
```

The ISO is a hybrid image: it boots via UEFI and legacy BIOS.

## 2. Boot the installer

Boot from the USB stick. A full-screen installer starts automatically.

Headless machine? The console prints a random root password. Log in over
SSH (`ssh root@<ip>`, the installer advertises itself over mDNS as
`_expanse-installer._tcp`) and run `expanse install --tui`.

## 3. Walk through the installer

1. **Welcome** — a hardware summary.
2. **Disks** — select target disks with space. 1 disk = single (a btrfs
   system partition + the remainder as LVM); 2+ = mirror (the first two
   disks carry a btrfs RAID1 system partition, every disk's remainder —
   including theirs — joins the LVM data pool as a plain, unmirrored PV).
   Disks holding data are marked `CONTAINS DATA` and require explicit
   confirmation.
3. **Network** — DHCP (recommended) or static.
4. **SSH key** — paste a key, or `gh:<username>` to fetch from GitHub.
5. **Review** — the full plan. Type `INSTALL` to proceed.
6. **Progress** — live per-stage log. Takes under 10 minutes.

When it finishes you get your **node ID** (a UUID). Reboot and remove the
USB stick.

## Unattended install (no TUI)

Write a config file and run:

```yaml
# expanse-install.yaml
version: 1
disks:
  layout: auto          # auto | single | mirror
  devices: [/dev/sda]   # empty = use every disk
network:
  mode: dhcp
ssh:
  authorized_keys:
    - ssh-ed25519 AAAA...
timezone: UTC
```

```sh
expanse install --config /tmp/expanse-install.yaml --force
```

- `--dry-run` prints every command that would run and touches nothing —
  use it first.
- Without `--force` the installer **refuses to touch a disk that contains
  data** and exits without modifying it.
- `--target-flake REF` installs from a specific flake instead of the one
  embedded in the ISO.

## 4. First boot

The node boots to `multi-user.target`, generates its identity (UUID +
Ed25519 keypair) in `/persist/expanse/identity`, and comes up on the
network via DHCP + mDNS. Log in with your SSH key:

```sh
ssh root@expanse-<node-id-prefix>.local
```

Verify:

```sh
expanse version
expanse node info
btrfs subvolume list /    # @root @nix @persist @log
vgs expanse                # the LVM data pool DRBD volumes are backed by
```

## Re-installing / recovery

Boot the installer USB again and re-run. The installer is idempotent:
same config → same system. Node identity survives (it lives in
`/persist`), unless you destroy the system partition yourself.

## Troubleshooting

| Symptom | Fix |
|---|---|
| "refusing to wipe non-empty disk" | That is intentional. Add `--force` only if you mean it. |
| Install fails at `partition` | Check `lsblk`; the target may be the USB stick itself. |
| Root not wiped on reboot | The `@root-blank` snapshot is missing — `expanse doctor storage` flags it; re-install to restore it, never hand-edit the subvolume layout. |
| Clock warnings in logs | Old CMOS battery; chrony fixes time after 3 steps. |
| Want to see what changed on reboot | `journalctl -u expanse-impermanence-check` |
