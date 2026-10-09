# Spike (Phase 1 B3 / D5): can DRBD 9 take a new node-id on a live resource,
# or must a rebuilt replica recreate the resource? Observes and reports; it
# does not gate. Retired once vol-drbd (E5) covers rebuild.
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
    environment.systemPackages = [ pkgs.python3 ];
  };
  lint = pkgs.runCommand "vol-drbd-nodeid-spike-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    python3 -c 'import sys; compile(open(sys.argv[1]).read(), "testscript", "exec")' ${./python/vol_drbd_nodeid_spike_main.py}
    touch $out
  '';
in
{
  name = "expanse-vol-drbd-nodeid-spike";

  nodes = {
    n1 = nodeCommon;
    n2 = nodeCommon;
    n3 = nodeCommon;
    n4 = nodeCommon;
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./python/vol_drbd_nodeid_spike_main.py}
  '';
}
