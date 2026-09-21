# Phase 1 B4: the volume runtime on real LVM and DRBD, four VMs.
{ self }:
{ pkgs, lib, ... }:
let
  volctl = pkgs.buildGoModule {
    pname = "volctl";
    version = "0";
    src = lib.cleanSource ../../.;
    vendorHash = null; # deps are vendored in ./vendor
    env.CGO_ENABLED = "0";
    subPackages = [ "test/volctl" ];
    doCheck = false;
  };
  lint = pkgs.runCommand "vol-runtime-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    python3 -c 'import sys; compile(open(sys.argv[1]).read(), "testscript", "exec")' ${./python/vol_runtime_main.py}
    touch $out
  '';
  node = { config, ... }: {
    boot.extraModulePackages = [ config.boot.kernelPackages.drbd ];
    services.drbd.enable = true;
    services.drbd.config = ''
      global { usage-count no; }
      include "/etc/drbd.d/*.res";
    '';
    systemd.services.drbd.wantedBy = lib.mkForce [ ];
    services.lvm.enable = true;
    services.lvm.boot.thin.enable = true;
    environment.systemPackages = [ pkgs.lvm2 pkgs.thin-provisioning-tools volctl ];
    virtualisation.emptyDiskImages = [ 1024 ];
    virtualisation.memorySize = 1024;
    networking.firewall.allowedTCPPortRanges = [{ from = 7800; to = 8799; }];
  };
in
{
  name = "expanse-vol-runtime";

  nodes = {
    n1 = node;
    n2 = node;
    n3 = node;
    n4 = node;
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./python/vol_runtime_main.py}
  '';
}
