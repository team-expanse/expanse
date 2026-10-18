#!/usr/bin/env bash
# Checks docs/INSTALL.md §6 (moving /nix onto an LV) in QEMU, on a node iso-check.sh installed.
# Usage: nix-lv-check.sh <iso-check workdir>. Logs to <workdir>/nix-lv-check.log. Run inside `nix develop`.
# shellcheck disable=SC2016 # the single-quoted commands run on the node and expand there
set -euo pipefail

work=$(readlink -f "${1:?usage: nix-lv-check.sh <iso-check workdir>}")
port=${PORT:-2225}
[[ -f $work/b0.qcow2 && -f $work/vars.fd ]] || { echo "no installed node in $work"; exit 1; }
exec > >(tee "$work/nix-lv-check.log") 2>&1

ovmf=$(nix build --inputs-from "$(dirname "$0")/.." --no-link --print-out-paths nixpkgs#OVMF.fd)/FV
key=$work/id_ed25519
qpid=

step() { echo "== $(date +%T) $*"; }
fail() {
  echo "FAIL: $*"
  exit 1
}
ssh_() {
  ssh -q -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 \
    -o ServerAliveInterval=5 -o ServerAliveCountMax=3 \
    -i "$key" -p "$port" root@127.0.0.1 "$@"
}
alive() { [[ -n $qpid ]] && kill -0 "$qpid" 2>/dev/null; }
up() {
  for _ in $(seq 1 100); do
    ssh_ true 2>/dev/null && return 0
    alive || fail "qemu exited"
    sleep 3
  done
  fail "no SSH on port $port"
}
down() {
  for _ in $(seq 1 60); do
    ssh_ true 2>/dev/null || return 0
    sleep 2
  done
  fail "node did not go down"
}
stop() {
  [[ -n $qpid ]] || return 0
  kill "$qpid" 2>/dev/null || true
  wait "$qpid" 2>/dev/null || true
  qpid=
}
trap stop EXIT

qemu-system-x86_64 -enable-kvm -machine q35 -cpu host -smp 2 -m 4G \
  -drive if=pflash,format=raw,readonly=on,file="$ovmf/OVMF_CODE.fd" \
  -drive if=pflash,format=raw,file="$work/vars.fd" \
  -drive file="$work/b0.qcow2",if=none,id=b0,format=qcow2 -device virtio-blk-pci,drive=b0,serial=b0,bootindex=1 \
  -drive file="$work/b1.qcow2",if=none,id=b1,format=qcow2 -device virtio-blk-pci,drive=b1,serial=b1,bootindex=2 \
  -nic "user,model=virtio-net-pci,restrict=on,hostfwd=tcp:127.0.0.1:$port-:22" -display none \
  -serial file:"$work/serial-nix-lv.log" &
qpid=$!

step "before: /nix on the system partition"
up
ssh_ 'findmnt -no SOURCE /nix; df -h /nix | tail -1; vgs expanse'

step "make the volume (§6 step 1)"
ssh_ 'lvcreate -q -L 16G -n nix expanse && mkfs.btrfs -q -L nix /dev/expanse/nix' || fail "make the volume"

step "configure and build the next boot (§6 steps 2-3)"
ssh_ 'cat > /etc/nixos/nix-lv.nix' <<'NIX'
{ lib, ... }:
{
  boot.initrd.services.lvm.enable = true;
  fileSystems."/nix" = lib.mkForce {
    device = "/dev/expanse/nix";
    fsType = "btrfs";
    options = [ "compress=zstd" "noatime" ];
    neededForBoot = true;
  };
}
NIX
# Not part of §6: kernel and initrd messages on the serial log, to debug a boot that fails.
ssh_ 'echo "{ boot.kernelParams = [ \"console=ttyS0,115200\" ]; }" > /etc/nixos/serial-console.nix'
ssh_ 'sed -i "s|^    ./hardware-configuration.nix$|&\n    ./nix-lv.nix\n    ./serial-console.nix|" /etc/nixos/configuration.nix && grep -q nix-lv.nix /etc/nixos/configuration.nix' ||
  fail "imports edit"
ssh_ 'nixos-rebuild boot 2>&1 | tail -1' || fail "nixos-rebuild boot"

step "copy the store and reboot (§6 step 4)"
ssh_ 'set -e
  systemctl stop nix-daemon.socket nix-daemon.service
  mkdir -p /mnt/nix && mount -o compress=zstd /dev/expanse/nix /mnt/nix
  cp -a /nix/. /mnt/nix/
  umount /mnt/nix' || fail "copy"
ssh_ 'systemctl reboot --no-block' || true
down

step "after reboot: /nix on the LV"
up
src=$(ssh_ 'findmnt -no SOURCE /nix')
echo "$src"
[[ $src == /dev/mapper/expanse-nix* ]] || fail "/nix is on $src"
ssh_ 'df -h /nix | tail -1; systemctl is-system-running || true; expanse ctl node health | head -1'
ssh_ 'for _ in $(seq 1 60); do curl -sf localhost:18080 && exit 0; sleep 3; done; exit 1' || fail "block not back"
echo
ssh_ 'cd /tmp && nixos-rebuild build 2>&1 | tail -1' || fail "rebuild on the LV"

step "free the old copy (§6 step 5)"
ssh_ 'set -e
  dev=$(findmnt -no SOURCE /persist | sed "s/\[.*//")
  mkdir -p /mnt/top && mount -o subvolid=5 "$dev" /mnt/top
  btrfs subvolume delete /mnt/top/@nix >/dev/null
  btrfs subvolume create /mnt/top/@nix >/dev/null
  umount /mnt/top
  df -h /persist | tail -1' || fail "free old store"
ssh_ 'systemctl reboot --no-block' || true
down
up
[[ $(ssh_ 'findmnt -no SOURCE /nix') == /dev/mapper/expanse-nix* ]] || fail "/nix not on the LV after the cleanup reboot"
ssh_ 'for _ in $(seq 1 60); do curl -sf localhost:18080 && exit 0; sleep 3; done; exit 1' || fail "block not back"
echo
step "PASS"
