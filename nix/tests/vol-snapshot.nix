# §6 vol-snapshot (G6.13): write A, snapshot, write B, restore the
# snapshot — content must be back to A, B must be gone, and every
# replica must agree (the restore-triggered resync of the other two
# replicas must actually converge, not just the primary's own zvol).
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
    expanse.agent.raftAdvertise = "192.168.1.${toString idx}:7444";
    boot.kernelModules = [ "nbd" ];
    virtualisation.memorySize = 2048;
    virtualisation.diskSize = 12 * 1024;
    networking.firewall.interfaces.exp0.allowedTCPPorts = [ 9440 ];
    environment.systemPackages = with pkgs; [ zfs nbd python3 ];
  };
  lint = pkgs.runCommand "vol-snapshot-lint" { } ''
    ${pkgs.python3}/bin/python3 -m py_compile ${./python/vol_snapshot_main.py}
    cat ${./cluster-common.py} ${./python/vol_snapshot_main.py} | ${pkgs.python3}/bin/python3 -c 'import sys; compile(sys.stdin.read(), "testscript", "exec")'
    touch $out
  '';
in
{
  name = "expanse-vol-snapshot";

  nodes = {
    n1 = { ... }: nodeCommon 1;
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./python/vol_snapshot_main.py}
  '';
}
