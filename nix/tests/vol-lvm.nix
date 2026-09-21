# Phase 1 A3: the lvm wrapper against real LVM, on two scratch disks.
{ self }:
{ pkgs, lib, ... }:
let
  lvmtest = pkgs.buildGoModule {
    pname = "lvm-vm-test";
    version = "0";
    src = lib.cleanSource ../../.;
    vendorHash = null; # deps are vendored in ./vendor
    env.CGO_ENABLED = "0";
    doCheck = false;
    buildPhase = ''
      runHook preBuild
      go test -c -tags lvmvm -o lvm.test ./internal/storage/lvm
      runHook postBuild
    '';
    installPhase = ''
      install -Dm755 lvm.test $out/bin/lvm.test
    '';
  };
  lint = pkgs.runCommand "vol-lvm-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    python3 -c 'import sys; compile(open(sys.argv[1]).read(), "testscript", "exec")' ${./python/vol_lvm_main.py}
    touch $out
  '';
in
{
  name = "expanse-vol-lvm";

  nodes.n1 = { ... }: {
    services.lvm.enable = true;
    services.lvm.boot.thin.enable = true;
    environment.systemPackages = [ pkgs.lvm2 pkgs.thin-provisioning-tools ];
    virtualisation.emptyDiskImages = [ 1024 512 ];
    virtualisation.memorySize = 1024;
  };

  testScript = ''
    # ${lint}
    ${builtins.replaceStrings [ "@lvmtest@" ] [ "${lvmtest}" ] (builtins.readFile ./python/vol_lvm_main.py)}
  '';
}
