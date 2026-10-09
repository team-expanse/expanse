# Probe (Phase 4 D1): does LIO's rtslib-fb accept a backstore pointed at a
# DRBD device this node holds Secondary, the way real per-node ALUA
# (PHASE-04-TASKS.md D1, Stream B) needs? DRBD documents Secondary as
# refusing ALL local I/O, even reads -- this probe settles, before any
# Stream B code is written, whether that refusal happens at backstore
# *creation* (open()/size query, which would make real ALUA impossible
# over single-primary DRBD and force D1's VIP/SINGLETON fallback) or only
# at actual data I/O (which real ALUA never needs on a Standby path under
# correct multipath behavior). Observations only; nothing is asserted --
# same role vol-drbd-verify-probe.nix played for Phase 1 C4b. Not a gate.
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "iscsi-lio-drbd-secondary-probe-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    python3 -c 'import sys; compile(open(sys.argv[1]).read(), "testscript", "exec")' ${./python/iscsi_lio_drbd_secondary_probe.py}
    touch $out
  '';
  drbdConf = ''
    global { usage-count no; }
    resource r0 {
      device /dev/drbd0 minor 0;
      disk /dev/vdb;
      meta-disk internal;
      net { protocol C; }
      on n1 { node-id 0; address 192.168.1.1:7789; }
      on n2 { node-id 1; address 192.168.1.2:7789; }
      connection-mesh { hosts n1 n2; }
    }
  '';
  node = { config, ... }: {
    imports = [ ../modules/drbd.nix ];
    boot.extraModulePackages = [ config.boot.kernelPackages.drbd ];
    services.drbd.enable = true;
    services.drbd.config = drbdConf;
    systemd.services.drbd.wantedBy = lib.mkForce [ ];
    networking.firewall.allowedTCPPorts = [ 7789 ];
    virtualisation.emptyDiskImages = [ 512 ];
    virtualisation.memorySize = 1024;
    environment.systemPackages = [ pkgs.targetcli-fb ];
  };
in
{
  name = "expanse-iscsi-lio-drbd-secondary-probe";
  nodes = { n1 = node; n2 = node; };
  testScript = ''
    # ${lint}
    ${builtins.readFile ./python/iscsi_lio_drbd_secondary_probe.py}
  '';
}
