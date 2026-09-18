# §6 vol-full-restart (G6.10): write 2 GiB, checksum, hard-stop all 3
# nodes, start all 3, assert the volume becomes Healthy within 60 s and
# the checksum matches — the volume must survive a full cluster
# restart, not just a single-node failover.
{ self }:
{ pkgs, lib, ... }:
let
  nodeCommon = idx: {
    imports = [
      self.nixosModules.expanse
      ../modules/storage-test.nix
    ];
    nixpkgs.overlays = [
      (final: prev: { expanse = self.packages.${prev.system}.expanse; })
    ];
    expanse.node.enable = true;
    expanse.agent.enable = true;
    expanse.hostId = "0000000${toString idx}";
    expanse.hostname = "n${toString idx}";
    expanse.agent.exvolPool = "volumes";
    expanse.storage-test.enable = true;
    expanse.storage-test.poolSizeMB = 6144;
    # Stable raft advertise across the restart — every node comes back
    # at the SAME address, not a re-guessed per-boot one.
    expanse.agent.raftAdvertise = "192.168.1.${toString idx}:7444";
    boot.kernelModules = [ "nbd" ];
    virtualisation.memorySize = 2048;
    virtualisation.diskSize = 12 * 1024;
    networking.firewall.interfaces.exp0.allowedTCPPorts = [ 9440 ];
    environment.systemPackages = with pkgs; [ zfs nbd python3 ];
  };
  lint = pkgs.runCommand "vol-full-restart-lint" { } ''
    ${pkgs.python3}/bin/python3 -m py_compile ${./python/vol_full_restart_main.py}
    cat ${./cluster-common.py} ${./python/vol_full_restart_main.py} | ${pkgs.python3}/bin/python3 -c 'import sys; compile(sys.stdin.read(), "testscript", "exec")'
    touch $out
  '';
in
{
  name = "expanse-vol-full-restart";

  nodes = {
    n1 = { ... }: nodeCommon 1;
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./python/vol_full_restart_main.py}
  '';
}
