# X1 A/B twin of nix/perf/containers: the same idle, volume-free 3-node cluster
# (python/idle_main.py) on 4 GB / 2 core VMs, so its CPU can be compared with bare metal.
{ self }:
{ pkgs, lib, ... }:
let
  budgetsJson = pkgs.runCommand "node-idle-budgets.json" { nativeBuildInputs = [ pkgs.yq-go ]; } ''
    yq -o=json '.budgets' ${../../test/perf/budgets.yaml} > $out
  '';
  budgetsPy = "BUDGETS = json.loads(open('${budgetsJson}').read())\n";
  lint = pkgs.runCommand "node-idle-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    cd ${./python}
    { cat ${./cluster-common.py} vol_perf_lib.py node_overhead.py; printf '%s\n' ${lib.escapeShellArg budgetsPy}; cat idle_main.py; } \
      | python3 -c 'import sys; compile(sys.stdin.read(), "testscript", "exec")'
    touch $out
  '';
  nodeCommon = idx: {
    imports = [ self.nixosModules.expanse ];
    nixpkgs.overlays = [
      (final: prev: { expanse = self.packages.${prev.system}.expanse; })
    ];
    expanse.node.enable = true;
    expanse.agent.enable = true;
    expanse.hostId = "0000000${toString idx}";
    expanse.hostname = "n${toString idx}";
    expanse.agent.raftAdvertise = "192.168.1.${toString idx}:7444";
    virtualisation.memorySize = 4096;
    virtualisation.cores = 2;
  };
in
{
  name = "expanse-node-idle";

  nodes = {
    n1 = { ... }: nodeCommon 1;
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./python/vol_perf_lib.py}
    ${builtins.readFile ./python/node_overhead.py}
    ${budgetsPy}
    ${builtins.readFile ./python/idle_main.py}
  '';
}
