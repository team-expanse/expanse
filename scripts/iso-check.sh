#!/usr/bin/env bash
# Release check for an installer ISO in QEMU: unattended mirror install, then a first boot with no
# network (health, offline nixos-rebuild, a block), then a reboot. Usage: iso-check.sh <iso> [workdir].
# Logs to <workdir>/iso-check.log; exits non-zero on the first failed step. Run inside `nix develop`.
# shellcheck disable=SC2016 # the single-quoted loops run on the node and expand there
set -euo pipefail

iso=$(readlink -f "${1:?usage: iso-check.sh <iso> [workdir]}")
work=$(readlink -f "${2:-$(mktemp -d)}")
port=${PORT:-2224}
mkdir -p "$work"
exec > >(tee "$work/iso-check.log") 2>&1

ovmf=$(nix build --inputs-from "$(dirname "$0")/.." --no-link --print-out-paths nixpkgs#OVMF.fd)/FV
# A UNIX socket path must stay under 108 bytes, which a deep workdir can exceed.
sockdir=$(mktemp -d "${XDG_RUNTIME_DIR:-/tmp}/iso-check.XXXXXX")
sock=$sockdir/serial.sock
key=$work/id_ed25519
[[ -f $key ]] || ssh-keygen -q -t ed25519 -N '' -f "$key"
qpid=

step() { echo "== $(date +%T) $*"; }
fail() {
  echo "FAIL: $*"
  exit 1
}
ssh_() {
  ssh -q -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 \
    -i "$key" -p "$port" root@127.0.0.1 "$@"
}
up() {
  for _ in $(seq 1 100); do
    ssh_ true 2>/dev/null && return 0
    alive || fail "qemu exited"
    sleep 3
  done
  fail "no SSH on port $port"
}
alive() { [[ -n $qpid ]] && kill -0 "$qpid" 2>/dev/null; }
stop() {
  [[ -n $qpid ]] || return 0
  kill "$qpid" 2>/dev/null || true
  wait "$qpid" 2>/dev/null || true
  qpid=
}
trap 'stop; rm -rf "$sockdir"' EXIT

# boot <nic-options> <qemu args...>: start the VM on the two disks in the background.
boot() {
  local nic=$1
  shift
  qemu-system-x86_64 -enable-kvm -machine q35 -cpu host -smp 2 -m 4G \
    -drive if=pflash,format=raw,readonly=on,file="$ovmf/OVMF_CODE.fd" \
    -drive if=pflash,format=raw,file="$work/vars.fd" \
    -drive file="$work/b0.qcow2",if=none,id=b0,format=qcow2 -device virtio-blk-pci,drive=b0,serial=b0,bootindex=1 \
    -drive file="$work/b1.qcow2",if=none,id=b1,format=qcow2 -device virtio-blk-pci,drive=b1,serial=b1,bootindex=2 \
    -nic "user,model=virtio-net-pci$nic,hostfwd=tcp:127.0.0.1:$port-:22" -display none "$@" &
  qpid=$!
}

step "install $(basename "$iso")"
rm -f "$work"/b0.qcow2 "$work"/b1.qcow2
cp "$ovmf/OVMF_VARS.fd" "$work/vars.fd" && chmod u+w "$work/vars.fd"
qemu-img create -q -f qcow2 "$work/b0.qcow2" 32G
qemu-img create -q -f qcow2 "$work/b1.qcow2" 32G
boot "" -drive file="$iso",if=none,id=cd0,media=cdrom,readonly=on -device ide-cd,drive=cd0,bootindex=9 \
  -chardev "socket,id=s0,path=$sock,server=on,wait=off,logfile=$work/serial-install.log" -serial chardev:s0
for _ in $(seq 1 100); do
  grep -q "automatic login" "$work/serial-install.log" 2>/dev/null && break
  alive || fail "qemu exited"
  sleep 3
done
# The live ISO logs root in on ttyS0; install the key through it.
python3 - "$sock" "$(cat "$key.pub")" <<'PY'
import socket, sys, time
s = socket.socket(socket.AF_UNIX)
s.connect(sys.argv[1])
s.sendall(f"mkdir -p /root/.ssh && echo '{sys.argv[2]}' > /root/.ssh/authorized_keys\n".encode())
time.sleep(1)
PY
up
ssh_ "cat > /tmp/cfg.yaml" <<CFG
version: 1
hostname: isocheck
disks:
  layout: mirror
  devices: [/dev/vda, /dev/vdb]
network:
  mode: dhcp
ssh:
  authorized_keys:
    - $(cat "$key.pub")
timezone: UTC
CFG
ssh_ 'expanse install --config /tmp/cfg.yaml --force' || fail "install"
stop

step "first boot, no network"
boot ",restrict=on" -serial file:"$work/serial-boot.log"
up
ssh_ 'expanse version; systemctl is-system-running || true; expanse ctl node health | head -1'
ssh_ 'test -f /etc/nixos/expanse/nix/modules/expanse-node.nix' || fail "no node source in /etc/nixos/expanse"
step "nixos-rebuild build, offline"
ssh_ 'cd /tmp && nixos-rebuild build 2>&1 | tail -3' || fail "nixos-rebuild build"
step "cluster and a block"
ssh_ 'systemctl stop expansed && expanse cluster init --expect 1 >/dev/null && systemctl start expansed'
ssh_ 'for _ in $(seq 1 30); do expanse cluster status 2>/dev/null | grep -q "quorum:    1/1" && exit 0; sleep 2; done; exit 1' ||
  fail "no one-node quorum"
ssh_ 'expanse cluster status | head -3'
ssh_ 'cat > /root/hello.yaml' <<'YAML'
apiVersion: expanse.io/v1
kind: Block
metadata:
  name: hello
  namespace: default
spec:
  type: util/echo
  replicas: 1
  config:
    port: 18080
    body: "hello from iso-check"
YAML
ssh_ 'expanse ctl block apply -f /root/hello.yaml'
ssh_ 'for _ in $(seq 1 60); do curl -sf localhost:18080 && exit 0; sleep 3; done; exit 1' || fail "block never served"
ssh_ 'touch /etc/iso-check-marker; systemctl reboot' || true

step "after reboot"
# QEMU restarts the guest in place: wait for SSH to go away, then to come back.
for _ in $(seq 1 60); do
  ssh_ true 2>/dev/null || break
  sleep 2
done
up
ssh_ 'test ! -e /etc/iso-check-marker' || fail "root was not wiped on reboot"
ssh_ 'expanse ctl node health | head -1'
ssh_ 'for _ in $(seq 1 60); do curl -sf localhost:18080 && exit 0; sleep 3; done; exit 1' || fail "block not back after reboot"
step "PASS"
