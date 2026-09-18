# §6 vol-degraded (G6.11, G6.12): R=3, kill 1 secondary — the volume
# must report Degraded and stay fully readable+writable; kill a 2nd —
# it must report ReadOnly, reads keep working from the primary's local
# copy, and writes fail fast with EIO (never hang, reusing T09's
# lease/quorum-loss EIO guarantee, proven here end to end). Restore
# both and the volume must recover to Healthy with all 3 replicas
# checksum-equal.
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
    # Stable raft advertise across a crash/restore cycle — same
    # rationale as vol-durability.nix: a hard-killed node must rejoin
    # at the SAME address, not a re-guessed per-boot one.
    expanse.agent.raftAdvertise = "192.168.1.${toString idx}:7444";
    boot.kernelModules = [ "nbd" ];
    virtualisation.memorySize = 2048;
    virtualisation.diskSize = 12 * 1024;
    networking.firewall.interfaces.exp0.allowedTCPPorts = [ 9440 ];
    environment.systemPackages = with pkgs; [ zfs nbd python3 ];
  };
  lint = pkgs.runCommand "vol-degraded-lint" { } ''
    ${pkgs.python3}/bin/python3 -m py_compile ${./python/vol_degraded_main.py}
    cat ${./cluster-common.py} ${./python/vol_degraded_main.py} | ${pkgs.python3}/bin/python3 -c 'import sys; compile(sys.stdin.read(), "testscript", "exec")'
    touch $out
  '';
in
{
  name = "expanse-vol-degraded";

  nodes = {
    n1 = { ... }: nodeCommon 1;
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./python/vol_degraded_main.py}
  '';
}
