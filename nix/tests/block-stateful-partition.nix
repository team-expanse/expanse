# A stateful block's node is cut off while its unit holds a replication-2 volume open: the block
# moves, yet no two nodes are ever DRBD Primary at once. Scenario: python/block_stateful_partition.py.
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "block-stateful-partition-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    for f in ${./cluster-common.py} ${./block-common.py} ${./python/vol_cluster.py} ${./python/vol_role_recorder.py} ${./python/block_stateful_partition.py}; do
      python3 -c 'import sys; compile(open(sys.argv[1]).read(), sys.argv[1], "exec")' $f
    done
    touch $out
  '';
  rolesDir = pkgs.runCommand "vol-roles" { } ''
    mkdir $out
    cp ${./python/vol_roles.py} $out/vol_roles.py
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
    expanse.agent.period = "5s";
    expanse.agent.controllerPeriod = "5s";
    expanse.agent.blocksCatalog = ../blocks;
    expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
    environment.etc."expanse/blocks-flake".source = ../blocks-flake;
    environment.systemPackages = [ pkgs.curl pkgs.jq ];
    virtualisation.memorySize = 1536;
  };
in
{
  name = "expanse-block-stateful-partition";

  nodes = {
    n1 = { ... }: nodeCommon 1;
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
  };

  testScript = ''
    # ${lint}
    import importlib
    import sys

    sys.path.insert(0, "${rolesDir}")
    roles = importlib.import_module("vol_roles")

    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./block-common.py}
    ${builtins.readFile ./python/vol_cluster.py}
    ${builtins.readFile ./python/vol_role_recorder.py}
    ${builtins.readFile ./python/block_stateful_partition.py}
  '';
}
