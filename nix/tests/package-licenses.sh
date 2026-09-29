#!/usr/bin/env bash
# Checks that the expanse package ships its own LICENSE and NOTICE and every vendored module's licence.
# Usage: package-licenses.sh PACKAGE_OUT vendor/modules.txt
set -euo pipefail
pkg=$1 modules=$2
dir=$pkg/share/licenses/expanse
fails=0
missing() { echo "missing: $1"; fails=$((fails + 1)); }

for f in LICENSE NOTICE THIRD-PARTY.md; do
  [[ -s $dir/$f ]] || missing "$dir/$f"
done
count=0
while read -r hash mod _; do
  [[ $hash == "#" && $mod != "=>" ]] || continue
  count=$((count + 1))
  compgen -G "$dir/vendor/$mod/[LlCc][IiOo][CcPp]*" >/dev/null || missing "licence for $mod"
  grep -qF -- "$mod" "$dir/THIRD-PARTY.md" 2>/dev/null || missing "$mod in THIRD-PARTY.md"
done <"$modules"

((count > 0)) || missing "any module in $modules"
if ((fails > 0)); then echo "package-licenses: $fails problems"; exit 1; fi
echo "package-licenses: LICENSE, NOTICE and $count vendored module licences present"
