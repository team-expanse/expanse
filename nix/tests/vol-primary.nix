# Phase 1 B5: the promotion gate on real DRBD, three VMs.
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
  lint = pkgs.runCommand "vol-primary-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    for f in ${./python/vol_common.py} ${./python/vol_primary_main.py}; do
      python3 -c 'import sys; compile(open(sys.argv[1]).read(), sys.argv[1], "exec")' $f
    done
    touch $out
  '';
  node = { config, ... }: {
    imports = [ ../modules/drbd.nix ];
    boot.extraModulePackages = [ config.boot.kernelPackages.drbd ];
    services.drbd.enable = true;
    services.drbd.config = ''
      global { usage-count no; }
      include "/etc/drbd.d/*.res";
    '';
    systemd.services.drbd.wantedBy = lib.mkForce [ ];
    services.lvm.enable = true;
    services.lvm.boot.thin.enable = true;
    environment.systemPackages = [ pkgs.lvm2 pkgs.thin-provisioning-tools pkgs.e2fsprogs pkgs.iptables volctl ];
    virtualisation.emptyDiskImages = [ 1024 ];
    virtualisation.memorySize = 1024;
    networking.firewall.allowedTCPPortRanges = [{ from = 9500; to = 10499; }];
  };
in
{
  name = "expanse-vol-primary";

  nodes = {
    n1 = node;
    n2 = node;
    n3 = node;
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./python/vol_common.py}
    ${builtins.readFile ./python/vol_primary_main.py}
  '';
}
