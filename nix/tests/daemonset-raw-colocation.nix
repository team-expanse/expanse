# PHASE-04-TASKS.md Stream A: a DAEMONSET block bound to storage must place
# only on nodes holding a healthy replica of it (D2), and a raw
# (filesystem: none) storage entry must be handed the bare device, never
# formatted or mounted (D3). The first VM test to combine DAEMONSET with
# bound storage at all.
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "daemonset-raw-colocation-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    for f in ${./cluster-common.py} ${./block-common.py} ${./python/vol_cluster.py} ${./python/daemonset_raw_colocation.py}; do
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
  name = "expanse-daemonset-raw-colocation";

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
    ${builtins.readFile ./python/daemonset_raw_colocation.py}
  '';
}
