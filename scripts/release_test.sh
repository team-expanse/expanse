#!/usr/bin/env bash
# Tests for release.sh: pure helpers directly, then a dry run against stub nix/gh in a scratch repo.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
# shellcheck source-path=SCRIPTDIR source=release.sh
source "$here/release.sh"
fails=0
check() { # check NAME WANT GOT
  if [[ "$2" != "$3" ]]; then printf 'FAIL %s\n want: %q\n got:  %q\n' "$1" "$2" "$3"; fails=$((fails + 1)); fi
}
expect_out() { # expect_out PATTERN: the dry run's output mentions PATTERN
  grep -qF -- "$1" <<<"$out" || { printf 'FAIL output lacks %q:\n%s\n' "$1" "$out"; fails=$((fails + 1)); }
}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
cat >"$tmp/CHANGELOG.md" <<'MD'
# Changelog

## 1.2.0 - 2026-10-01

### Fixed

- A "quoted" thing.
- A wrapped item that
  continues `--here` and
  - ends.

A wrapped
paragraph.

## 1.1.9 - 2026-09-30

- Older.
MD

# Wrapped lines are joined: GitHub renders each newline in release notes as a break.
check notes-body $'### Fixed\n\n- A "quoted" thing.\n- A wrapped item that continues `--here` and - ends.\n\nA wrapped paragraph.' "$(changelog_notes 1.2.0 "$tmp/CHANGELOG.md")"
check notes-last '- Older.' "$(changelog_notes 1.1.9 "$tmp/CHANGELOG.md")"
check notes-missing '' "$(changelog_notes 9.9.9 "$tmp/CHANGELOG.md")"
check date 2026-10-01 "$(changelog_date 1.2.0 "$tmp/CHANGELOG.md")"
check title 'Expanse 1.2.0: faster "boots"' "$(release_title 1.2.0 'release: 1.2.0 -- faster "boots"')"
check title-plain 'Expanse 1.2.0' "$(release_title 1.2.0 'release: 1.2.0')"
check json-escape 'a\"b\\c' "$(json_escape 'a"b\c')"
check repo-ssh team-expanse/expanse "$(github_repo git@github.com:team-expanse/expanse.git)"
check repo-https team-expanse/expanse "$(github_repo https://github.com/team-expanse/expanse)"

# Dry run end to end: a scratch repo with a tag, a stub nix that "builds" a tiny ISO, and a stub gh.
repo=$tmp/repo stub=$tmp/bin site=$tmp/site
mkdir -p "$repo/nix" "$stub" "$site/tools"
cp "$tmp/CHANGELOG.md" "$repo/"
echo '"1.2.0"' >"$repo/nix/version.nix"
git -C "$repo" init -q -b main
git -C "$repo" add -A
git -C "$repo" -c user.name=t -c user.email=t@t commit -q -m 'release: 1.2.0 -- faster boots'
git -C "$repo" tag v1.2.0
git -C "$repo" remote add origin git@github.com:team-expanse/expanse.git
cat >"$stub/nix" <<'SH'
#!/usr/bin/env bash
args=("$@"); for i in "${!args[@]}"; do [[ ${args[i]} == -o ]] && out=${args[i+1]}; done
if [[ -n ${out:-} ]]; then mkdir -p "$out/iso"; echo iso >"$out/iso/expanse-1.2.0-x86_64-linux.iso"; fi
echo "nix $*" >>"$STUB_LOG"
SH
cat >"$stub/gh" <<'SH'
#!/usr/bin/env bash
echo "gh $*" >>"$STUB_LOG"
SH
printf '#!/bin/sh\n' >"$site/tools/sync-release"
chmod +x "$stub"/* "$site/tools/sync-release"

export STUB_LOG=$tmp/log
out=$(cd "$repo" && PATH="$stub:$PATH" EXPANSE_WEBSITE=$site "$here/release.sh" --dry-run 2>&1) || { echo "$out"; fails=$((fails + 1)); }
dist=$repo/dist/1.2.0
sum=$(echo iso | sha256sum | cut -d' ' -f1)
check manifest-sha "\"sha256\": \"$sum\"," "$(grep '"sha256"' "$dist/release.json" | sed 's/^ *//')"
check manifest-url '"url": "https://github.com/team-expanse/expanse/releases/download/v1.2.0/expanse-1.2.0-x86_64-linux.iso",' \
  "$(grep '"url"' "$dist/release.json" | sed 's/^ *//')"
check sums "$sum  expanse-1.2.0-x86_64-linux.iso" "$(cat "$dist/SHA256SUMS")"
check notes-file "$(changelog_notes 1.2.0 "$tmp/CHANGELOG.md")" "$(cat "$dist/notes.md")"
check no-gh-in-dry-run '' "$(grep '^gh ' "$tmp/log" || true)"
expect_out 'would run: git push origin v1.2.0'
expect_out 'would run: gh release create v1.2.0'
expect_out "would run: $site/tools/sync-release $dist"

# Refuses a version whose tag is missing.
git -C "$repo" tag -d v1.2.0 >/dev/null
if (cd "$repo" && PATH="$stub:$PATH" "$here/release.sh" --dry-run >/dev/null 2>&1); then echo "FAIL ran without a tag"; fails=$((fails + 1)); fi

if ((fails > 0)); then echo "release_test: $fails failed"; exit 1; fi
echo "release_test: ok"
