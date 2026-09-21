# Phase 1 E4 (X4): a replica that missed writes catches up by bitmap, bounded in bytes and time.
# The scenario is python/vol_resync_incremental_main.py.
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "vol-resync-incremental-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    for f in ${./cluster-common.py} ${./python/vol_cluster.py} ${./python/vol_resync_incremental_main.py}; do
      python3 -c 'import sys; compile(open(sys.argv[1]).read(), sys.argv[1], "exec")' $f
    done
    touch $out
  '';
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
    expanse.storage-test.enable = true;
    expanse.agent.raftAdvertise = "192.168.1.${toString idx}:7444";
    virtualisation.memorySize = 2048;
  };
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
    ${builtins.readFile ./python/vol_cluster.py}
    ${builtins.readFile ./python/vol_resync_incremental_main.py}
  '';
}
