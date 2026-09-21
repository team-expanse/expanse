# Probe (Phase 1 C4b): observed DRBD behaviour of online verify and invalidate that the
# `volume verify` and `volume resync` design relies on. Not a gate.
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "vol-drbd-verify-probe-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    python3 -c 'import sys; compile(open(sys.argv[1]).read(), "testscript", "exec")' ${./python/vol_drbd_verify_probe_main.py}
    touch $out
  '';
  drbdConf = ''
    global { usage-count no; }
    resource r0 {
      device /dev/drbd0 minor 0;
      disk /dev/vdb;
      meta-disk internal;
      net { protocol C; verify-alg sha1; }
      on n1 { node-id 0; address 192.168.1.1:7789; }
      on n2 { node-id 1; address 192.168.1.2:7789; }
      on n3 { node-id 2; address 192.168.1.3:7789; }
      connection-mesh { hosts n1 n2 n3; }
    }
  '';
  node = { config, ... }: {
    boot.extraModulePackages = [ config.boot.kernelPackages.drbd ];
    services.drbd.enable = true;
    services.drbd.config = drbdConf;
    systemd.services.drbd.wantedBy = lib.mkForce [ ];
    networking.firewall.allowedTCPPorts = [ 7789 ];
    virtualisation.emptyDiskImages = [ 512 ];
    virtualisation.memorySize = 1024;
  };
in
{
  name = "expanse-vol-drbd-verify-probe";
  nodes = { n1 = node; n2 = node; n3 = node; };
  testScript = ''
    # ${lint}
    ${builtins.readFile ./python/vol_drbd_verify_probe_main.py}
  '';
}
