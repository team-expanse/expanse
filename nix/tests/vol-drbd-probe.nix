# Probe (Phase 1 B4): observed drbdadm behaviour the volume runtime relies on.
# Not a gate; run it to re-observe after a DRBD upgrade.
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "vol-drbd-probe-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    python3 -c 'import sys; compile(open(sys.argv[1]).read(), "testscript", "exec")' ${./python/vol_drbd_probe_main.py}
    touch $out
  '';
in
{
  name = "expanse-vol-drbd-probe";

  nodes.n1 = { config, ... }: {
    imports = [ ../modules/drbd.nix ];
    boot.extraModulePackages = [ config.boot.kernelPackages.drbd ];
    services.drbd.enable = true;
    services.drbd.config = ''
      global { usage-count no; }
      include "/etc/drbd.d/*.res";
    '';
    systemd.services.drbd.wantedBy = lib.mkForce [ ];
    virtualisation.emptyDiskImages = [ 256 ];
    virtualisation.memorySize = 1024;
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./python/vol_drbd_probe_main.py}
  '';
}
