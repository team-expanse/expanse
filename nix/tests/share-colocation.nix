# PHASE-03-TASKS.md Stream A1: a SINGLETON block bound to a volume by name
# must always run on the same node as that volume's DRBD primary — proven
# on initial placement, and again after a hard node kill, not by
# scheduling coincidence. The first VM test to deploy a block with bound
# storage at all; combines storage-test.nix (Phase 1's DRBD/LVM substrate)
# with blocksCatalog (Phase 04's block runtime).
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "share-colocation-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    for f in ${./cluster-common.py} ${./block-common.py} ${./python/vol_cluster.py} ${./python/share_colocation.py}; do
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
    # Tight reconcile period: both the block controller's placement and
    # the storage controller's movePrimaryForBlock need to converge
    # within the test's timeouts.
    expanse.agent.period = "5s";
    expanse.agent.controllerPeriod = "5s";
    expanse.agent.blocksCatalog = ../blocks;
    expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
    environment.etc."expanse/blocks-flake".source = ../blocks-flake;
    environment.systemPackages = [ pkgs.curl ];
    virtualisation.memorySize = 1536;
  };
in
{
  name = "expanse-share-colocation";

  nodes = {
    n1 = { ... }: nodeCommon 1;
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./block-common.py}
    ${builtins.readFile ./python/vol_cluster.py}
    ${builtins.readFile ./python/share_colocation.py}
  '';
}
