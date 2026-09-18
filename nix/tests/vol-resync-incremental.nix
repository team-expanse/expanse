# §6 vol-resync-incremental (G6.6, G6.7): write 5 GiB, take a secondary
# offline, write 100 MiB more, bring it back, and prove the resync that
# catches it up is an incremental `zfs send -i` (bytes received over exp0
# < 500 MiB) that completes inside 60 s and lands byte-correct.
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
    # The volume is 10 GiB and zvols are thick-provisioned (refreservation
    # = volsize, no -s flag) — the pool needs room for the full 10 GiB
    # reservation plus the 5 GiB + 100 MiB actually written, plus slack
    # for resync snapshots.
    expanse.storage-test.poolSizeMB = 12288;
    expanse.agent.raftAdvertise = "192.168.1.${toString idx}:7444";
    boot.kernelModules = [ "nbd" ];
    # rpool (vda) hosts /, /nix (the full expanse+zfs+python3 closure) AND
    # /persist (raft state) together — this test runs several minutes
    # longer than other vol-*.nix tests under sustained write load, giving
    # raft's own log/journald more real time to grow; headroom here avoids
    # rpool exhaustion taking /persist read-only mid-run (distinct from
    # storage-test.nix's separate "volumes" scratch pool on vdb, sized by
    # poolSizeMB above). The actual root cause of every earlier failure
    # here (a protocol.Secondary memory leak, and VM disk images landing
    # on a 6.3 GiB-capped /run/user tmpfs) is fixed; this is modest
    # headroom, not a workaround.
    virtualisation.memorySize = 2048;
    virtualisation.diskSize = 16 * 1024;
    networking.firewall.interfaces.exp0.allowedTCPPorts = [ 9440 ];
    environment.systemPackages = with pkgs; [ zfs nbd python3 ];
  };
  # py_compile at BUILD time: a python syntax error fails the check
  # build, not a multi-minute VM run.
  lint = pkgs.runCommand "vol-resync-incremental-lint" { } ''
    ${pkgs.python3}/bin/python3 -m py_compile ${./python/vol_resync_incremental_main.py}
    cat ${./cluster-common.py} ${./python/vol_resync_incremental_main.py} | ${pkgs.python3}/bin/python3 -c 'import sys; compile(sys.stdin.read(), "testscript", "exec")'
    touch $out
  '';
in
{
  name = "expanse-vol-resync-incremental";

  nodes = {
    n1 = { ... }: nodeCommon 1;
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./python/vol_resync_incremental_main.py}
  '';
}
