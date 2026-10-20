#!/usr/bin/env bash
# Upgrades this node to an Expanse release by hand: download, verify the signed SHA256SUMS, replace
# /etc/nixos/expanse, build with the release's binary cache and switch. For nodes before 1.12, which
# have no Upgrades page; see docs/UPGRADE.md, "From 1.11 or earlier". Run as root.
set -euo pipefail

# The release public key (internal/upgrade/release-key.pub), in Nix's name:base64 form.
release_key=expanse-release-1:AnEv4AWuSplcqvtjB9OmACQbT6qfPSv82MjwzBSaLFM=

usage() {
  cat <<'EOF'
Usage: upgrade-node.sh [--base-url URL] [--key NAME:BASE64] [--nixos-dir DIR] [--cache-dir DIR] [--yes] VERSION

Moves this node to release VERSION (e.g. 1.12.0). Drain the node first
(expanse ctl node drain <hostname>) and upgrade one node at a time.

  --base-url URL   release downloads, one directory per tag
                   (default https://github.com/team-expanse/expanse/releases/download)
  --key KEY        public key the release is signed with (default: the Expanse release key)
  --nixos-dir DIR  the node's NixOS config, holding expanse/ (default /etc/nixos)
  --cache-dir DIR  where downloads and the log are kept (default /var/cache/expanse/upgrade)
  --yes            skip the "is this node drained?" question
EOF
}

die() { echo "upgrade-node: $*" >&2; exit 1; }
say() { echo "upgrade-node: $*"; }

# key_pem KEY prints a Nix-format ed25519 public key as the PEM openssl reads.
key_pem() {
  [[ $(printf %s "${1#*:}" | base64 -d 2>/dev/null | wc -c) == 32 ]] ||
    die "release key $1 is not an ed25519 public key"
  echo "-----BEGIN PUBLIC KEY-----"
  { printf '\x30\x2a\x30\x05\x06\x03\x2b\x65\x70\x03\x21\x00'; printf %s "${1#*:}" | base64 -d; } | base64 -w0
  printf '\n-----END PUBLIC KEY-----\n'
}

# openssl_ runs openssl, from nixpkgs when the node has none (1.11 nodes do not).
openssl_() {
  if command -v openssl >/dev/null; then openssl "$@"; return; fi
  nix-shell -p openssl --run "openssl $(printf '%q ' "$@")"
}

# verify_sums DIR: SHA256SUMS in DIR is signed by $key.
verify_sums() {
  key_pem "$key" >"$1/release-key.pem"
  base64 -d "$1/SHA256SUMS.sig" >"$1/SHA256SUMS.sig.bin" || die "SHA256SUMS.sig is not base64"
  openssl_ pkeyutl -verify -pubin -inkey "$1/release-key.pem" -rawin \
    -in "$1/SHA256SUMS" -sigfile "$1/SHA256SUMS.sig.bin" >/dev/null ||
    die "SHA256SUMS is not signed by ${key%%:*}; not upgrading"
}

listed() { awk -v n="$2" '$2 == n || $2 == "*" n { found = 1 } END { exit !found }' "$1/SHA256SUMS"; }

fetch() { # fetch DIR NAME: download a release asset into DIR
  curl -fsSL --retry 3 -o "$1/$2" "$url/$2" || die "download $url/$2 failed"
}

# check_sum DIR NAME: NAME matches its SHA256SUMS line.
check_sum() {
  (cd "$1" && grep -E "  \*?$2\$" SHA256SUMS | sha256sum -c --quiet -) || die "$2 does not match SHA256SUMS"
}

source_version() { grep -v '^#' "$1/nix/version.nix" | tr -d '"[:space:]'; }

# install_source TARBALL: unpack as expanse/, keeping the current one as expanse.prev.
install_source() {
  local next=$nixos/expanse.next
  rm -rf "$next" "$nixos/expanse-$ver" && tar -xzf "$1" -C "$nixos" && mv "$nixos/expanse-$ver" "$next"
  [[ $(source_version "$next") == "$ver" ]] || die "the tarball does not hold release $ver"
  rm -rf "$nixos/expanse.prev"
  fresh=1
  if [[ -d $nixos/expanse ]]; then mv "$nixos/expanse" "$nixos/expanse.prev" && fresh=0; fi
  mv "$next" "$nixos/expanse"
  # 1.10.0 imported the installer's copy at /etc/expanse/flake/, which nodes do not have.
  if grep -q '/etc/expanse/flake/' "$nixos/configuration.nix"; then
    cp "$nixos/configuration.nix" "$nixos/configuration.nix.pre-$ver"
    sed -i 's|/etc/expanse/flake/|./expanse/|g' "$nixos/configuration.nix"
    say "configuration.nix now imports ./expanse/ (was /etc/expanse/flake/; kept as configuration.nix.pre-$ver)"
  fi
}

restore_source() {
  rm -rf "$nixos/expanse"
  if [[ $fresh == 0 ]]; then mv "$nixos/expanse.prev" "$nixos/expanse"; fi
}

# reboot_needed BOOTED NEW: the new system's kernel, initrd or modules differ from the booted ones.
reboot_needed() {
  local f
  for f in kernel initrd kernel-modules; do
    [[ $(readlink -f "$1/$f") == $(readlink -f "$2/$f") ]] || return 0
  done
  return 1
}

main() {
  local base=https://github.com/team-expanse/expanse/releases/download yes=0
  local cache_dir=/var/cache/expanse/upgrade
  key=$release_key nixos=/etc/nixos
  while (($#)); do
    case $1 in
      --base-url) base=${2:?}; shift ;; --key) key=${2:?}; shift ;; --nixos-dir) nixos=${2:?}; shift ;;
      --cache-dir) cache_dir=${2:?}; shift ;;
      --yes) yes=1 ;; -h | --help) usage; exit 0 ;; -*) usage >&2; exit 2 ;; *) ver=$1 ;;
    esac
    shift
  done
  [[ ${ver:-} =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$ ]] || { usage >&2; exit 2; }
  [[ -f $nixos/configuration.nix ]] || die "no $nixos/configuration.nix; is this an Expanse node?"
  url=$base/v$ver
  local work=$cache_dir/$ver
  mkdir -p "$work"
  exec > >(tee "$work/upgrade-node.log") 2>&1
  say "upgrading $(hostname) from $(expanse version 2>/dev/null | head -1 || echo unknown) to $ver; log: $work/upgrade-node.log"
  if [[ $yes == 0 ]]; then
    read -r -p "Is this node drained (expanse ctl node drain $(hostname)) and the cluster otherwise healthy? [y/N] " ok
    [[ $ok == [yY]* ]] || die "stopped; drain the node first"
  fi

  say "verifying the release signature"
  fetch "$work" SHA256SUMS
  fetch "$work" SHA256SUMS.sig
  verify_sums "$work"
  local tarball=expanse-$ver.tar.gz cache subst=()
  cache=expanse-$ver-cache-$(uname -m)-linux.tar
  listed "$work" "$tarball" || die "SHA256SUMS does not list $tarball"
  fetch "$work" "$tarball" && check_sum "$work" "$tarball"
  if listed "$work" "$cache"; then
    fetch "$work" "$cache" && check_sum "$work" "$cache"
    rm -rf "$work/cache" && mkdir "$work/cache" && tar -xf "$work/$cache" -C "$work/cache"
    subst=(--option extra-substituters "file://$work/cache" --option extra-trusted-public-keys "$key")
  fi

  say "installing the $ver source as $nixos/expanse"
  install_source "$work/$tarball"
  say "building the system (substitutes from cache.nixos.org and the release cache)"
  local sys
  if ! sys=$(nix-build "$nixos/expanse/nix/node-system.nix" -A config.system.build.toplevel --no-out-link "${subst[@]}"); then
    restore_source
    die "the build failed; the previous source is back in $nixos/expanse"
  fi
  say "switching to $sys"
  nix-env -p /nix/var/nix/profiles/system --set "$sys"
  "$sys/bin/switch-to-configuration" switch || die "switch-to-configuration failed; the new system is the boot default, the old one is still in the boot menu"
  if reboot_needed /run/booted-system "$sys"; then
    say "the kernel, initrd or kernel modules changed: reboot this node to finish, before upgrading the next one"
  fi
  say "done: $(expanse version | head -1). Wait for the cluster to be healthy, then: expanse ctl node uncordon $(hostname)"
}

if [[ ${BASH_SOURCE[0]} == "$0" ]]; then main "$@"; fi
