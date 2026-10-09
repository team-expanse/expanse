# Probe (bug #2): does a diskless third DRBD member give a live 2-replica resource a majority
# quorum, so a cut-off primary stops writing instead of diverging? Scenario in python/.
{ self }:
{ pkgs, lib, ... }:
let
  nodeCommon = { config, ... }: {
    imports = [ ../modules/drbd.nix ];
    boot.extraModulePackages = [ config.boot.kernelPackages.drbd ];
    services.drbd.enable = true;
    services.drbd.config = ''
      global { usage-count no; }
      include "/etc/drbd.d/*.res";
    '';
    # No resource exists until the test writes one.
    systemd.services.drbd.wantedBy = lib.mkForce [ ];
    virtualisation.emptyDiskImages = [ 256 ];
    virtualisation.memorySize = 1024;
    networking.firewall.allowedTCPPorts = [ 7789 ];
  };
  lint = pkgs.runCommand "vol-drbd-tiebreaker-probe-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    python3 -c 'import sys; compile(open(sys.argv[1]).read(), "testscript", "exec")' ${./python/vol_drbd_tiebreaker_probe_main.py}
    touch $out
  '';
in
{
  name = "expanse-vol-drbd-tiebreaker-probe";

  nodes = {
    n1 = nodeCommon;
    n2 = nodeCommon;
    n3 = nodeCommon;
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./python/vol_drbd_tiebreaker_probe_main.py}
  '';
}
