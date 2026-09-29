#!/usr/bin/env bash
# Publishes the tagged release in nix/version.nix to GitHub with its installer ISO, then syncs the website.
# Usage: scripts/release.sh [--dry-run] [--draft] [--yes]
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: scripts/release.sh [--dry-run] [--draft] [--yes]

Builds the ISO for the version in nix/version.nix (HEAD must be tag v<version>),
writes dist/<version>/{SHA256SUMS,notes.md,release.json}, pushes the tag, creates
or updates the GitHub release with the ISO attached, and runs the website hook
$EXPANSE_WEBSITE/tools/sync-release dist/<version> (default ../expanse-website).

  --dry-run  build and stage everything, but push, publish and sync nothing
  --draft    create the GitHub release as a draft
  --yes      skip the "verified in QEMU?" prompt
EOF
}

die() { echo "release: $*" >&2; exit 1; }

# changelog_notes VERSION FILE prints the body of "## VERSION - date", trimmed.
changelog_notes() {
  awk -v v="$1" '
    $0 ~ "^## " { if (on) exit; on = ($2 == v); next }
    on { buf = buf $0 "\n" }
    END { sub(/^\n+/, "", buf); sub(/\n+$/, "", buf); printf "%s", buf }
  ' "$2"
}

changelog_date() { awk -v v="$1" '$1 == "##" && $2 == v { print $4; exit }' "$2"; }

# release_title VERSION SUBJECT turns "release: 1.2.0 -- faster boots" into "Expanse 1.2.0: faster boots".
release_title() {
  local rest=${2#*-- }
  [[ "$rest" == "$2" ]] && { echo "Expanse $1"; return; }
  echo "Expanse $1: $rest"
}

json_escape() { local s=${1//\\/\\\\}; printf '%s' "${s//\"/\\\"}"; }

# github_repo URL prints owner/name for an ssh or https GitHub remote.
github_repo() {
  local r=${1%.git}
  r=${r#git@github.com:}
  echo "${r#https://github.com/}"
}

# run prints a command under --dry-run, otherwise runs it.
run() {
  if [[ $dry_run == 1 ]]; then echo "would run: $*"; else "$@"; fi
}

write_manifest() { # write_manifest FILE: every field comes from main's globals
  cat >"$1" <<EOF
{
  "version": "$ver",
  "tag": "$tag",
  "date": "$date",
  "title": "$(json_escape "$title")",
  "release_url": "https://github.com/$repo/releases/tag/$tag",
  "notes": "notes.md",
  "iso": {
    "name": "$iso_name",
    "url": "https://github.com/$repo/releases/download/$tag/$iso_name",
    "sha256": "$sha",
    "size": $size
  }
}
EOF
}

publish() { # creates the release, or refreshes its assets and notes if it exists
  local assets=("$iso" "$dist/SHA256SUMS" "$dist/release.json")
  if [[ $dry_run == 0 ]] && gh release view "$tag" --repo "$repo" >/dev/null 2>&1; then
    gh release upload "$tag" "${assets[@]}" --repo "$repo" --clobber
    gh release edit "$tag" --repo "$repo" --title "$title" --notes-file "$dist/notes.md"
    return
  fi
  local draft=()
  [[ $draft_flag == 1 ]] && draft=(--draft)
  run gh release create "$tag" "${assets[@]}" --repo "$repo" --verify-tag \
    --title "$title" --notes-file "$dist/notes.md" "${draft[@]}"
}

main() {
  dry_run=0 draft_flag=0 yes=0
  for a in "$@"; do
    case $a in
      --dry-run) dry_run=1 ;; --draft) draft_flag=1 ;; --yes) yes=1 ;;
      -h | --help) usage; exit 0 ;; *) usage >&2; exit 2 ;;
    esac
  done

  cd "$(git rev-parse --show-toplevel)"
  ver=$(grep -v '^#' nix/version.nix | tr -d '"[:space:]')
  tag=v$ver
  git rev-parse -q --verify "refs/tags/$tag" >/dev/null || die "no tag $tag; tag the release commit first"
  [[ $(git rev-parse "$tag^{commit}") == $(git rev-parse HEAD) ]] || die "HEAD is not $tag; check it out first"
  [[ -z $(git status --porcelain --untracked-files=no) ]] || die "working tree has uncommitted changes"

  date=$(changelog_date "$ver" CHANGELOG.md)
  [[ -n $date ]] || die "CHANGELOG.md has no '## $ver - <date>' section"
  title=$(release_title "$ver" "$(git log -1 --format=%s "$tag")")
  repo=$(github_repo "$(git remote get-url origin)")
  dist=$PWD/dist/$ver
  mkdir -p "$dist"
  changelog_notes "$ver" CHANGELOG.md >"$dist/notes.md"

  echo "release: building the $ver ISO"
  nix build .#iso -o "result-iso-$ver" -L
  nix build .#checks.x86_64-linux.iso-version --no-link
  iso=$(echo "result-iso-$ver/iso/expanse-$ver-"*.iso)
  [[ -f $iso ]] || die "no expanse-$ver-*.iso in result-iso-$ver/iso"
  iso_name=$(basename "$iso")
  sha=$(sha256sum "$iso" | cut -d' ' -f1)
  size=$(stat -L -c %s "$iso")
  echo "$sha  $iso_name" >"$dist/SHA256SUMS"
  write_manifest "$dist/release.json"
  echo "release: $iso_name ($size bytes) sha256 $sha, staged in $dist"

  if [[ $dry_run == 0 && $yes == 0 ]]; then
    read -r -p "Has $(readlink -f "result-iso-$ver") passed install, first boot and reboot in QEMU? [y/N] " ok
    [[ $ok == [yY]* ]] || die "stopped before publishing"
  fi
  if [[ $dry_run == 1 ]] || ! git ls-remote --exit-code --tags origin "refs/tags/$tag" >/dev/null 2>&1; then
    run git push origin "$tag"
  fi
  publish

  local site=${EXPANSE_WEBSITE:-../expanse-website}
  if [[ -x $site/tools/sync-release ]]; then
    run "$site/tools/sync-release" "$dist"
  else
    echo "release: no $site/tools/sync-release; the website was not updated"
  fi
  echo "release: done: https://github.com/$repo/releases/tag/$tag"
}

if [[ ${BASH_SOURCE[0]} == "$0" ]]; then main "$@"; fi
