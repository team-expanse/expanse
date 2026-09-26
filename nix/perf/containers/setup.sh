#!/usr/bin/env bash
# Phase 11, Stream A (X1): provisions and runs the container-based real-hardware(-adjacent)
# CPU/RSS/failover measurement. Run as root: `sudo ./setup.sh`. See ./README.md for the
# design and ../../.plan/ARCHITECTURE.md §8 for the budget this is measuring against.
#
# What this touches on the host, all reversible via ./teardown.sh:
#   - wipes and repartitions-as-LVM-PV /dev/sdb, /dev/sdc, /dev/sdd (nothing else)
#   - adds one `imports` line to /etc/nixos/configuration.nix (backed up first)
#   - runs `nixos-rebuild switch` (rollback: `nixos-rebuild switch --rollback`)
#   - creates /var/lib/expanse-perf/{n1,n2,n3}-persist on the host
set -euo pipefail

REPO="/home/jaredm/expanse"
MODULE="$REPO/nix/perf/containers/host-containers.nix"
CONF=/etc/nixos/configuration.nix
DISKS=(/dev/sdb /dev/sdc /dev/sdd)
NAMES=(n1 n2 n3)
PERSIST_ROOT=/var/lib/expanse-perf
FORCE=${FORCE:-0}

if [[ $EUID -ne 0 ]]; then
    echo "must run as root: sudo $0" >&2
    exit 1
fi

# Full output also goes to a logfile, so a long or failed run doesn't have to be
# pasted from terminal scrollback -- `tee` keeps it live on the terminal too. Deliberately
# NOT under $PERSIST_ROOT: teardown.sh rm -rf's that, which would erase the very log it's
# writing to (and every setup.sh log before it).
LOG_DIR=/var/log/expanse-perf
mkdir -p "$LOG_DIR"
LOG="$LOG_DIR/setup-$(date +%Y%m%dT%H%M%S).log"
exec > >(tee -a "$LOG") 2>&1
echo "logging full output to $LOG"

if [[ -e /etc/nixos/.configuration.nix.swp && $FORCE -ne 1 ]]; then
    echo "refusing to proceed: /etc/nixos/.configuration.nix.swp exists (a vim session may" >&2
    echo "be mid-edit on configuration.nix). Close it, or re-run with FORCE=1 if it's stale." >&2
    exit 1
fi

echo "== 1/6: checking the three disks are really empty =="
for d in "${DISKS[@]}"; do
    if [[ ! -b "$d" ]]; then
        echo "$d: not a block device" >&2
        exit 1
    fi
    if mount | grep -q "^$d"; then
        echo "$d: currently mounted, refusing" >&2
        exit 1
    fi
    found="$(lsblk -no FSTYPE,PARTTYPE "$d" 2>/dev/null | tr -d ' \n')"
    if [[ -n "$found" && $FORCE -ne 1 ]]; then
        echo "$d: not empty (lsblk reports FSTYPE/PARTTYPE data: $found)." >&2
        echo "Refusing to touch a disk that looks like it has something on it." >&2
        echo "Re-run with FORCE=1 only if you are certain this is safe to wipe." >&2
        exit 1
    fi
    echo "  $d: empty, OK"
done

echo "== 2/6: deactivating any stale VG and wiping signatures on the three disks =="
# A disk that still backs an ACTIVE VG (from a prior run -- device-mapper mappings
# persist independent of any container's own lifecycle, they don't get torn down just
# because the container that created them stops) makes wipefs fail outright: "Device
# or resource busy" -- found live. Stop the containers first (nothing should still
# have the VG open once they're down) and deactivate whatever VG the host itself can
# see on each disk -- host-level lvm2 commands can see it directly from the PV headers
# on disk, regardless of which container's own private lvm.conf originally created it,
# so this generically handles any leftover name (the old shared "expanse", or the
# current per-container "expanse-n<N>").
for n in "${NAMES[@]}"; do
    systemctl stop "container@$n.service" 2>/dev/null || true
done
# `vgchange -an <uuid>` doesn't work -- vgchange takes a VG name/tag, not a bare UUID
# ("Volume group '<uuid>' not found"), found live. And the *name* is ambiguous here (3
# disks each carrying a VG literally named "expanse" from before per-container VG
# naming existed -- "WARNING: VG name expanse is used by VGs <uuid1> and <uuid2>").
# Simplest reliable fix: remove the DM devices directly by name, bypassing VG-name
# resolution entirely. Tries every VG name this harness has ever used (old shared
# "expanse", current per-container "expanse-n<N>"), ignoring failures for combinations
# that don't exist.
#
# dmsetup escapes literal "-" in VG/LV names as "--" (single "-" is reserved as the
# VG/LV separator in the mapper name) -- confirmed live via `dmsetup info -c`: VG
# "expanse-n1" produces device "expanse--n1-pool", not "expanse-n1-pool".
#
# Rather than hardcode the pool/tdata/tmeta device names, enumerate whatever DM devices
# actually exist under each VG's prefix and remove them in repeated passes -- found live
# that the exact set is NOT fixed: once a real thin LV is created on the pool (this
# harness's own measurement run does exactly that), LVM's on-disk representation grows
# an additional "<pool>-tpool" internal target device plus one device per thin LV
# (e.g. "expanse--n1-vol--8abccc7313e498e7"), none of which the old hardcoded 3-name
# list knew about -- their pool was still open/busy, so wipefs kept failing "Device or
# resource busy" even after this loop "succeeded". A device with active dependents
# fails to remove and is silently skipped (`|| true`); each pass removes whatever was a
# pure leaf, freeing up its dependencies for the next pass, so this converges regardless
# of dependency depth or ordering without needing to know the exact device graph.
for vg in expanse expanse-n1 expanse-n2 expanse-n3; do
    dmvg="${vg//-/--}"
    for _ in 1 2 3 4 5 6; do
        remaining="$(dmsetup ls 2>/dev/null | awk -v p="^${dmvg}-" '$1 ~ p {print $1}')"
        [[ -z "$remaining" ]] && break
        while IFS= read -r dev; do
            [[ -n "$dev" ]] && dmsetup remove "$dev" 2>/dev/null || true
        done <<< "$remaining"
    done
done
for d in "${DISKS[@]}"; do
    wipefs -a "$d"
done

echo "== 3/6: pre-creating each container's /persist (host-side) =="
for n in "${NAMES[@]}"; do
    p="$PERSIST_ROOT/$n-persist"
    mkdir -p "$p"/{etc,var/lib/nixos,var/lib/systemd,root/.ssh}
    touch "$p/etc/machine-id"
    chmod 0700 "$p/root/.ssh"
    echo "  $p ready"
done
# host-containers.nix's containers.<name>.bindMounts only covers the dedicated data
# disk; /persist itself needs its own bindMounts entry too, added here rather than
# hardcoded in the .nix file so the host-side path stays a setup.sh concern.
cat > /etc/nixos/expanse-perf-persist-mounts.nix <<'EOF'
# Generated by nix/perf/containers/setup.sh -- do not hand-edit; see ../../nix/perf/containers/README.md.
{ ... }: {
  containers.n1.bindMounts."/persist" = { hostPath = "/var/lib/expanse-perf/n1-persist"; isReadOnly = false; };
  containers.n2.bindMounts."/persist" = { hostPath = "/var/lib/expanse-perf/n2-persist"; isReadOnly = false; };
  containers.n3.bindMounts."/persist" = { hostPath = "/var/lib/expanse-perf/n3-persist"; isReadOnly = false; };
}
EOF

echo "== 4/6: wiring /etc/nixos/configuration.nix =="
MARK="# >>> expanse X1 perf containers (nix/perf/containers/setup.sh) >>>"
if ! grep -qF "$MARK" "$CONF"; then
    cp "$CONF" "$CONF.bak-$(date +%s)"
    python3 - "$CONF" "$MODULE" "$MARK" <<'PYEOF'
import sys
conf_path, module_path, mark = sys.argv[1:4]
text = open(conf_path).read()
insert = f"    {mark}\n    {module_path}\n    /etc/nixos/expanse-perf-persist-mounts.nix\n    # <<< expanse X1 perf containers <<<\n"
idx = text.index("imports =")
idx = text.index("[", idx) + 1
text = text[:idx] + "\n" + insert + text[idx:]
open(conf_path, "w").write(text)
PYEOF
    echo "  added import lines (backup at $CONF.bak-*)"
else
    echo "  already wired, skipping"
fi

echo "== 5/6: nixos-rebuild switch =="
# host-containers.nix's builtins.getFlake needs the flakes experimental feature.
# /etc/nix/nix.conf (system-wide, what root's nixos-rebuild actually reads) has it
# empty -- only the invoking user's own ~/.config/nix/nix.conf grants it, which root
# under sudo does not inherit. Scoped to just this command, not written to any file.
NIX_CONFIG="experimental-features = nix-command flakes" nixos-rebuild switch

echo "== 6/6: (re)starting containers so they pick up whatever was just built =="
# `restart`, not `start`: containers.<name> (autoStart = false) registers and populates
# the machine the first time container@<name>.service itself starts -- `machinectl
# start` only works on an already-registered image and fails "Machine image '<name>'
# does not exist" before that (found live). And autoStart = false ALSO disables NixOS's
# own restart-on-config-change machinery (nixos-containers.nix's restartTriggers/
# restartIfChanged are set inside `optionalAttrs containerConfig.autoStart { ... }` --
# a no-op when it's false), so `nixos-rebuild switch` alone never restarts an already-
# running container to pick up a rebuilt config -- `systemctl start` on an
# already-active unit is *also* a no-op, silently leaving the OLD process running with
# stale capabilities/devices even though the new config built and switched correctly.
# Found live: this is exactly why the CAP_MKNOD fix never took effect on a container
# that was already running successfully from the bridge fix. `restart` is correct
# whether the unit is currently stopped or running.
for n in "${NAMES[@]}"; do
    systemctl restart "container@$n.service"
done

# systemd-run --machine= depends on the container's D-Bus machine-transport socket,
# which is not reliably up yet right after start -- found live ("Failed to connect to
# system scope bus via machine transport"). nsenter into the container's own PID
# namespace via its leader PID instead (the same mechanism `nixos-container run` itself
# uses, proven to work against a running container regardless of D-Bus state).
failed=0
for n in "${NAMES[@]}"; do
    ok=0
    for i in $(seq 1 180); do
        # Re-fetch the leader PID every iteration, not just once before the loop --
        # found live: querying it immediately after `restart` can race the container's
        # own startup (leader not registered with machinectl yet), leaving it empty for
        # the whole 180s wait even though the container comes up fine seconds later.
        leader="$(machinectl show "$n" -p Leader --value 2>/dev/null)"
        # is-system-running exits non-zero for "degraded" (0 means only "running"), so
        # this must stay an if-condition rather than a bare assignment to stay exempt
        # from set -e.
        if status="$(nsenter --target "$leader" --all -- systemctl is-system-running 2>&1)"; then
            rc=0
        else
            rc=$?
        fi
        # Progress breadcrumb for a slow or stuck boot, without spamming every second.
        if (( i % 10 == 0 )); then
            echo "  $n: [$(date +%T)] leader='$leader' rc=$rc status='$status'" >&2
        fi
        if [[ -n "$leader" ]] && [[ "$status" =~ running|degraded ]]; then
            ok=1
            break
        fi
        sleep 1
    done
    if [[ $ok -eq 1 ]]; then
        echo "  $n: up ($status)"
    else
        echo "  $n: NOT ready after 180s -- last seen: leader='$leader' rc=$rc status='$status'" >&2
        failed=1
    fi
done

echo
if [[ $failed -eq 0 ]]; then
    echo "All 3 containers up. Now run the measurement (also as root):"
else
    echo "One or more containers did NOT come up cleanly -- see above before running the measurement." >&2
fi
echo "  sudo python3 $REPO/nix/perf/containers/run.py"
[[ $failed -eq 0 ]]
