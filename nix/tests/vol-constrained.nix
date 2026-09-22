# Phase 1 E7 (X8): the whole-node budget from ARCHITECTURE.md §8 — a 4 GB RAM, 2 core node
# with one data disk still forms a cluster, brings a replicated volume healthy, keeps the
# agent's own idle overhead inside budget with that volume attached, and fails it over inside
# budget when its primary is hard-killed. The scenario is python/vol_constrained_main.py.
{ self }:
{ pkgs, lib, ... }:
let
  budgetsJson = pkgs.runCommand "vol-constrained-budgets.json" { nativeBuildInputs = [ pkgs.yq-go ]; } ''
    yq -o=json '.budgets' ${../../test/perf/budgets.yaml} > $out
  '';
  budgetsPy = "BUDGETS = json.loads(open('${budgetsJson}').read())\n";
  lint = pkgs.runCommand "vol-constrained-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    cd ${./python}
    { cat ${./cluster-common.py} vol_cluster.py vol_perf_lib.py; printf '%s\n' ${lib.escapeShellArg budgetsPy}; cat vol_constrained_main.py; } \
      | python3 -c 'import sys; compile(sys.stdin.read(), "testscript", "exec")'
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
    # The advertised minimum spec (ARCHITECTURE.md §8), not the looser sizing other vol-*
    # tests use for speed. One added scratch disk (see the scope-limit note in the plan for
    # why this and every other vol-* test also carries the framework's own boot disk).
    expanse.storage-test.diskSizeMB = 4096;
    expanse.storage-test.poolPercent = 80;
    expanse.agent.raftAdvertise = "192.168.1.${toString idx}:7444";
    virtualisation.memorySize = 4096;
    virtualisation.cores = 2;
  };
in
{
  name = "expanse-vol-constrained";

  nodes = {
    n1 = { ... }: nodeCommon 1;
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./python/vol_cluster.py}
    ${builtins.readFile ./python/vol_perf_lib.py}
    ${budgetsPy}
    ${builtins.readFile ./python/vol_constrained_main.py}
  '';
}
