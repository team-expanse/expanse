#!/usr/bin/env bash
# Tests for upgrade-node.sh against a signed fake release served over file:// and stub nix commands.
# Needs openssl: nix shell nixpkgs#openssl -c scripts/upgrade-node_test.sh
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
fixtures=$here/../nix/tests/fixtures
# shellcheck source-path=SCRIPTDIR source=upgrade-node.sh
source "$here/upgrade-node.sh"
fails=0
fail() { printf 'FAIL %s\n' "$*"; fails=$((fails + 1)); }

tmp=$(mktemp -d)
trap '[[ -n ${KEEP:-} ]] || rm -rf "$tmp"' EXIT
stub=$tmp/bin
mkdir -p "$stub"

# A tiny "system" the stub nix-build prints; its switch-to-configuration records that it ran.
sys=$tmp/system
mkdir -p "$sys/bin"
for f in kernel initrd kernel-modules; do ln -s "$(readlink -f /run/booted-system/$f)" "$sys/$f"; done
printf '#!/bin/sh\necho "$@" >>"%s/switched"\n' "$tmp" >"$sys/bin/switch-to-configuration"
cat >"$stub/nix-build" <<SH
#!/bin/sh
echo "\$@" >>"$tmp/nix-build.args"
[ -e "$tmp/build-fails" ] && exit 1
echo "$sys"
SH
printf '#!/bin/sh\necho "$@" >>"%s/nix-env.args"\n' "$tmp" >"$stub/nix-env"
printf '#!/bin/sh\necho "expanse 1.11.3 (stub)"\n' >"$stub/expanse"
chmod +x "$stub"/* "$sys/bin/switch-to-configuration"
export PATH=$stub:$PATH

test_key=$(cat "$fixtures/release-test-key.pub")
# sign FILE: base64 ed25519 signature with the test secret key, as `expanse upgrade sign` writes it.
sign() {
  local secret
  secret=$(cut -d: -f2 "$fixtures/release-test-key.sec")
  { echo "-----BEGIN PRIVATE KEY-----"
    { printf '\x30\x2e\x02\x01\x00\x30\x05\x06\x03\x2b\x65\x70\x04\x22\x04\x20'
      printf %s "$secret" | base64 -d | head -c 32; } | base64 -w0
    printf '\n-----END PRIVATE KEY-----\n'; } >"$tmp/test.sec.pem"
  openssl pkeyutl -sign -inkey "$tmp/test.sec.pem" -rawin -in "$1" | base64 -w0
  echo
}

# release VERSION: a signed release in $tmp/rel/vVERSION with a source tarball and a cache.
release() {
  local d=$tmp/rel/v$1 src=$tmp/src/expanse-$1
  rm -rf "$d" "$tmp/src" && mkdir -p "$d" "$src/nix" "$tmp/cachesrc/nar"
  echo "\"$1\"" >"$src/nix/version.nix"
  echo '{ }: { }' >"$src/nix/node-system.nix"
  tar -czf "$d/expanse-$1.tar.gz" -C "$tmp/src" "expanse-$1"
  echo StoreDir: /nix/store >"$tmp/cachesrc/nix-cache-info"
  tar -cf "$d/expanse-$1-cache-$(uname -m)-linux.tar" -C "$tmp/cachesrc" .
  (cd "$d" && sha256sum expanse-"$1"* >SHA256SUMS)
  sign "$d/SHA256SUMS" >"$d/SHA256SUMS.sig"
}

# node: a fresh /etc/nixos holding the 1.11.3 source.
node() {
  rm -rf "$tmp/nixos" "$tmp/cache" "$tmp"/*.args "$tmp/switched" "$tmp/build-fails"
  mkdir -p "$tmp/nixos/expanse/nix"
  echo '"1.11.3"' >"$tmp/nixos/expanse/nix/version.nix"
  echo '{ imports = [ ./expanse/nix/modules/expanse-node.nix ]; }' >"$tmp/nixos/configuration.nix"
}

upgrade() { # upgrade VERSION [ARGS...]: run the script; output in $out, status in $rc
  local v=$1
  shift
  rc=0
  out=$("$here/upgrade-node.sh" --base-url "file://$tmp/rel" --key "$test_key" --nixos-dir "$tmp/nixos" \
    --cache-dir "$tmp/cache" --yes "$@" "$v" 2>&1) || rc=$?
}

version_in() { tr -d '"' <"$tmp/nixos/$1/nix/version.nix"; }
expect_fail() { # expect_fail NAME PATTERN
  [[ $rc != 0 ]] || fail "$1: succeeded"
  grep -qF -- "$2" <<<"$out" || fail "$1: output lacks '$2': $out"
}

# The script's PEM conversion matches the one docs/UPGRADE.md shows for the release key.
grep -qF "$(key_pem "$release_key" | sed -n 2p)" "$here/../docs/UPGRADE.md" || fail "docs/UPGRADE.md lacks the release key PEM"
[[ $release_key == "$(cat "$here/../internal/upgrade/release-key.pub")" ]] ||
  fail "release_key differs from internal/upgrade/release-key.pub"

b=$tmp/booted n=$tmp/new
mkdir -p "$b" "$n"
for f in kernel initrd kernel-modules; do touch "$tmp/$f" "$tmp/$f.new"; ln -s "$tmp/$f" "$b/$f"; ln -s "$tmp/$f" "$n/$f"; done
reboot_needed "$b" "$n" && fail "reboot_needed: same kernel, initrd and modules"
ln -sf "$tmp/kernel-modules.new" "$n/kernel-modules"
reboot_needed "$b" "$n" || fail "reboot_needed: new kernel modules"

release 9.9.9
node
upgrade 9.9.9
[[ $rc == 0 ]] || fail "upgrade: rc $rc: $out"
[[ $(version_in expanse) == 9.9.9 && $(version_in expanse.prev) == 1.11.3 ]] || fail "upgrade: source not swapped"
grep -qF "extra-substituters file://$tmp/cache/9.9.9/cache --option extra-trusted-public-keys $test_key" "$tmp/nix-build.args" ||
  fail "upgrade: nix-build without the release cache: $(cat "$tmp/nix-build.args")"
grep -qF -- "--set $sys" "$tmp/nix-env.args" || fail "upgrade: profile not set"
[[ $(cat "$tmp/switched" 2>/dev/null) == switch ]] || fail "upgrade: no switch"
[[ -s $tmp/cache/9.9.9/upgrade-node.log ]] || fail "upgrade: no log"
grep -q "reboot this node" <<<"$out" && fail "upgrade: asks for a reboot with the booted kernel, initrd and modules"

node
upgrade 9.9.9 --key "$(cat "$here/../internal/upgrade/release-key.pub")"
expect_fail wrong-key "is not signed by expanse-release-1"
[[ $(version_in expanse) == 1.11.3 && ! -e $tmp/nixos/expanse.prev ]] || fail "wrong-key: source touched"

node
echo tampered >>"$tmp/rel/v9.9.9/SHA256SUMS"
upgrade 9.9.9
expect_fail tampered-sums "is not signed by"
release 9.9.9

node
echo junk >>"$tmp/rel/v9.9.9/expanse-9.9.9.tar.gz"
upgrade 9.9.9
expect_fail tampered-tarball "expanse-9.9.9.tar.gz does not match SHA256SUMS"
[[ $(version_in expanse) == 1.11.3 ]] || fail "tampered-tarball: source touched"
release 9.9.9

node
touch "$tmp/build-fails"
upgrade 9.9.9
expect_fail build-fails "the build failed"
[[ $(version_in expanse) == 1.11.3 && ! -e $tmp/nixos/expanse.prev ]] || fail "build-fails: source not restored"
[[ ! -e $tmp/switched ]] || fail "build-fails: switched anyway"

# 1.10.0: no expanse/ yet and imports from the installer's /etc/expanse/flake/.
node
rm -rf "$tmp/nixos/expanse"
echo '{ imports = [ /etc/expanse/flake/nix/modules/expanse-node.nix ]; }' >"$tmp/nixos/configuration.nix"
touch "$tmp/build-fails"
upgrade 9.9.9
[[ ! -e $tmp/nixos/expanse ]] || fail "fresh build-fails: new source left behind"
rm "$tmp/build-fails"
upgrade 9.9.9
[[ $rc == 0 && $(version_in expanse) == 9.9.9 ]] || fail "from-1.10.0: rc $rc: $out"
grep -qF './expanse/nix/modules/expanse-node.nix' "$tmp/nixos/configuration.nix" || fail "from-1.10.0: imports not rewritten"
[[ -f $tmp/nixos/configuration.nix.pre-9.9.9 ]] || fail "from-1.10.0: no configuration.nix backup"

upgrade 9.9 && [[ $rc == 2 ]] || fail "bad version: rc $rc"

((fails == 0)) || { echo "$fails failure(s)"; exit 1; }
echo PASS
