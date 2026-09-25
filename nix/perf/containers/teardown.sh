#!/usr/bin/env bash
# Reverses setup.sh: stops and destroys the 3 containers, removes the config-nix wiring,
# rebuilds, and wipes the three disks back to empty. Run as root: `sudo ./teardown.sh`.
set -euo pipefail

CONF=/etc/nixos/configuration.nix
DISKS=(/dev/sdb /dev/sdc /dev/sdd)
NAMES=(n1 n2 n3)
PERSIST_ROOT=/var/lib/expanse-perf

if [[ $EUID -ne 0 ]]; then
    echo "must run as root: sudo $0" >&2
    exit 1
fi

echo "== 1/5: stopping containers =="
for n in "${NAMES[@]}"; do
    systemctl stop "container@$n.service" 2>/dev/null || true
    machinectl terminate "$n" 2>/dev/null || true  # belt-and-suspenders if still registered
done

echo "== 2/5: removing config-nix wiring =="
MARK="# >>> expanse X1 perf containers (nix/perf/containers/setup.sh) >>>"
if grep -qF "$MARK" "$CONF"; then
    cp "$CONF" "$CONF.bak-teardown-$(date +%s)"
    python3 - "$CONF" "$MARK" <<'PYEOF'
import re, sys
conf_path, mark = sys.argv[1:3]
text = open(conf_path).read()
text = re.sub(
    r"\n *" + re.escape(mark) + r".*?<<< expanse X1 perf containers <<<\n",
    "\n", text, flags=re.S,
)
open(conf_path, "w").write(text)
PYEOF
fi
rm -f /etc/nixos/expanse-perf-persist-mounts.nix

echo "== 3/5: nixos-rebuild switch =="
# See setup.sh: root's nixos-rebuild doesn't inherit the invoking user's flakes config.
NIX_CONFIG="experimental-features = nix-command flakes" nixos-rebuild switch

echo "== 4/5: deactivating leftover DM devices and wiping the three disks =="
# Same fix as setup.sh's step 2: dmsetup escapes "-" in VG/LV names as "--", so the
# per-container VG "expanse-n1" maps to device "expanse--n1-pool" etc. Without
# deactivating these first, wipefs fails "Device or resource busy" -- found live.
for vg in expanse expanse-n1 expanse-n2 expanse-n3; do
    dmvg="${vg//-/--}"
    dmsetup remove "$dmvg-pool" 2>/dev/null || true
    dmsetup remove "$dmvg-pool_tdata" 2>/dev/null || true
    dmsetup remove "$dmvg-pool_tmeta" 2>/dev/null || true
done
for d in "${DISKS[@]}"; do
    wipefs -a "$d" || true
done

echo "== 5/5: removing host-side persist directories =="
rm -rf "$PERSIST_ROOT"

echo "Done. Containers destroyed, config rewired back, disks wiped clean."
echo "(configuration.nix backups are kept at $CONF.bak-* -- delete manually if not wanted.)"
