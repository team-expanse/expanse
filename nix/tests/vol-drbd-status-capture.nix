# Captures real `drbdsetup status --json` output for the internal/storage/drbd
# parser fixtures (Phase 1 B1). Not a gate: run it to regenerate the fixtures.
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
    systemd.services.drbd.wantedBy = lib.mkForce [ ];
    virtualisation.emptyDiskImages = [ 256 256 ];
    virtualisation.memorySize = 1024;
    networking.firewall.allowedTCPPorts = [ 7789 7790 ];
  };
  lint = pkgs.runCommand "vol-drbd-status-capture-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    python3 -c 'import sys; compile(open(sys.argv[1]).read(), "testscript", "exec")' ${./python/vol_drbd_status_capture_main.py}
    touch $out
  '';
in
{
  name = "expanse-vol-drbd-status-capture";

  nodes = {
    n1 = nodeCommon;
    n2 = nodeCommon;
    n3 = nodeCommon;
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./python/vol_drbd_status_capture_main.py}
  '';
}
