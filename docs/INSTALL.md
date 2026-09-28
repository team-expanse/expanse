# Installing Expanse

This guide takes a blank x86_64 or aarch64 machine to a running Expanse
node. It assumes no familiarity with Nix or btrfs. For the disk layout this
produces, the thin-pool policy, and enabling replicated volumes afterward,
see `docs/STORAGE.md`.

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
   disks each carry an ESP and a system partition, mirrored as md RAID1 so
   either disk alone boots; every disk's remainder — including theirs —
   joins the LVM data pool as a plain, unmirrored PV).
   Disks holding data are marked `CONTAINS DATA` and require explicit
   confirmation.
3. **Network** — DHCP (recommended) or static.
4. **SSH key** — paste a key, or `gh:<username>` to fetch from GitHub.
5. **Review** — the full plan, with a red list of what will be destroyed.
   Type `INSTALL` to proceed (the install starts on the last letter).
6. **Progress** — the current stage, elapsed time and a tail of the log
   (the whole log is in `/tmp/expanse-install.log`). Takes under 10 minutes.

When it finishes you get your **node ID** (a UUID), the machine's addresses
and the next steps: the web UI URL, where the admin password is logged and
how to form a one-node cluster. Reboot and remove the USB stick.

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
network via DHCP + mDNS.

The screen (tty1) shows the **host console**: the Expanse version, hostname
and node ID, every address with the web UI URL (`https://<ip>:8443`), the
cluster name, role and quorum (or "not in a cluster yet"), the node's
health, CPU, memory, disks and md mirror state, and the uptime. It refreshes
every few seconds and is read-only: it never offers a shell or shows a
secret. Press **Alt+F2** for a login shell (tty2), or use the serial console;
`expanse console --once` prints the same screen over SSH. It runs as
`expanse-console.service` and takes the place of the tty1 login.

Log in with your SSH key:

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

The VG exists but is otherwise empty: replicated volumes (the thin pool, DRBD) are an explicit
opt-in, not part of a fresh install. See `docs/STORAGE.md` to enable and operate them.

One installed node is already a usable cluster. Form it with the agent stopped (`cluster init`
refuses while `expansed` runs, since the running agent would never join the new cluster):

```sh
systemctl stop expansed
expanse cluster init --expect 1
systemctl start expansed
expanse cluster status     # quorum: 1/1
```

Volumes and blocks then run there. Everything on a single node has **no redundancy** until more nodes
join (`docs/CLUSTERING.md`, `docs/STORAGE.md` §8).

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
