#!/usr/bin/env bash
# Reverses setup.sh: stops and destroys the 3 containers, removes the config-nix wiring,
# and rebuilds. Run as root: `sudo ./teardown.sh`.
set -euo pipefail

CONF=/etc/nixos/configuration.nix
NAMES=(n1 n2 n3)
PERSIST_ROOT=/var/lib/expanse-perf

if [[ $EUID -ne 0 ]]; then
    echo "must run as root: sudo $0" >&2
    exit 1
fi

# Full output also goes to a logfile (see setup.sh for why this isn't under
# $PERSIST_ROOT, which this script itself rm -rf's).
LOG_DIR=/var/log/expanse-perf
mkdir -p "$LOG_DIR"
LOG="$LOG_DIR/teardown-$(date +%Y%m%dT%H%M%S).log"
exec > >(tee -a "$LOG") 2>&1
echo "logging full output to $LOG"

echo "== 1/4: stopping containers =="
for n in "${NAMES[@]}"; do
    systemctl stop "container@$n.service" 2>/dev/null || true
    machinectl terminate "$n" 2>/dev/null || true  # belt-and-suspenders if still registered
done

echo "== 2/4: removing config-nix wiring =="
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

echo "== 3/4: nixos-rebuild switch =="
# See setup.sh: root's nixos-rebuild doesn't inherit the invoking user's flakes config.
NIX_CONFIG="experimental-features = nix-command flakes" nixos-rebuild switch

echo "== 4/4: removing host-side persist directories =="
rm -rf "$PERSIST_ROOT"

echo "Done. Containers destroyed, config rewired back."
echo "(configuration.nix backups are kept at $CONF.bak-* -- delete manually if not wanted.)"
