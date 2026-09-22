# Phase 1 E5 (X2): storage perf budgets on the real stack (agent, LVM thin, DRBD 9), and the
# thin-vs-thick axis that settles D2. The scenario is python/vol_perf_main.py; the budget
# numbers come from test/perf/budgets.yaml, the single source of them.
{ self }:
{ pkgs, lib, ... }:
let
  budgetsJson = pkgs.runCommand "budgets.json" { nativeBuildInputs = [ pkgs.yq-go ]; } ''
    yq -o=json '.budgets' ${../../test/perf/budgets.yaml} > $out
  '';
  budgetsPy = "BUDGETS = json.loads(open('${budgetsJson}').read())\n";
  lint = pkgs.runCommand "vol-perf-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    cd ${./python}
    python3 vol_perf_lib_test.py
    { cat ${./cluster-common.py} vol_cluster.py vol_perf_lib.py; printf '%s\n' ${lib.escapeShellArg budgetsPy}; cat vol_perf_fio.py vol_perf_main.py; } \
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
    # The replicas and thin baselines fit in the pool; the thick LVs and a second, 1 MiB-chunk pool sit outside it.
    expanse.storage-test.diskSizeMB = 16384;
    expanse.storage-test.poolPercent = 60;
    expanse.agent.raftAdvertise = "192.168.1.${toString idx}:7444";
    virtualisation.memorySize = 2048;
    virtualisation.cores = 2;
    environment.systemPackages = with pkgs; [ fio iperf3 iputils procps ];
    networking.firewall.interfaces.exp0.allowedTCPPorts = [ 5201 ];
    networking.firewall.allowedTCPPorts = [ 5201 ];
  };
in
{
  name = "expanse-vol-perf";

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
    ${builtins.readFile ./python/vol_perf_fio.py}
    ${builtins.readFile ./python/vol_perf_main.py}
  '';
}
