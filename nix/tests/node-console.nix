# Host console: an installed node's tty1 shows the read-only `expanse console` screen
# (identity, addresses, cluster, health, mirror state) instead of a login, while tty2 and
# the serial console still log in. The scenario is python/node_console_main.py.
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "node-console-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    python3 -c 'import sys; compile(open(sys.argv[1]).read(), "testscript", "exec")' ${./python/node_console_main.py}
    touch $out
  '';
in
{
  name = "expanse-node-console";

  nodes.machine = { config, pkgs, ... }: {
    imports = [ self.nixosModules.expanse ];
    nixpkgs.overlays = [
      (final: prev: { expanse = self.packages.${prev.system}.expanse; })
    ];
    expanse.node.enable = true;
    expanse.agent.enable = true;
    expanse.hostId = "01234567";
    expanse.hostname = "console-node";

    # As expanse-node.nix configures an installed node: a login on the serial port too.
    boot.kernelParams = [ "console=ttyS0,115200n8" "console=tty1" ];

    # A mounted /persist, as on an installed node, so the identity (node ID) is generated
    # (test VMs take their mounts from virtualisation.fileSystems, not fileSystems).
    virtualisation.fileSystems."/persist" = { device = "none"; fsType = "tmpfs"; options = [ "mode=0755" ]; neededForBoot = true; };

    # Two spare disks to build a degraded md mirror from.
    boot.swraid.enable = true;
    boot.swraid.mdadmConf = "PROGRAM ${pkgs.coreutils}/bin/true";
    environment.systemPackages = [ pkgs.mdadm ];

    virtualisation.memorySize = 2048;
    virtualisation.cores = 2;
    virtualisation.emptyDiskImages = [ 512 512 ];
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./python/node_console_main.py}
  '';
}
