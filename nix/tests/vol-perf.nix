# §5 perf tests (Phase 06 T23): fio profiles on an exvol R=3 volume vs a
# raw local zvol with the same properties on the same VM, plus resync
# rate and primary-crash failover time, all checked against
# test/perf/budgets.yaml (the single source of the budget numbers).
# VM-only: needs a real ZFS pool, so it cannot live in test/perf's
# loopback harness.
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
    # 2 GiB exvol zvol + 2 GiB raw zvol, both fully reserved, plus resync
    # snapshots and headroom.
    expanse.storage-test.poolSizeMB = 12288;
    expanse.agent.raftAdvertise = "192.168.1.${toString idx}:7444";
    boot.kernelModules = [ "nbd" ];
    virtualisation.memorySize = 2048;
    virtualisation.diskSize = 16 * 1024;
    networking.firewall.interfaces.exp0.allowedTCPPorts = [ 9440 ];
    environment.systemPackages = with pkgs; [ zfs nbd python3 fio iputils ];
  };
  budgetsJson = pkgs.runCommand "budgets.json" { nativeBuildInputs = [ pkgs.yq-go ]; } ''
    yq -o=json '.budgets' ${../../test/perf/budgets.yaml} > $out
  '';
  budgetsPy = "BUDGETS = json.loads(open('${budgetsJson}').read())\n";
  lint = pkgs.runCommand "vol-perf-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    cd ${./python}
    python3 vol_perf_lib_test.py
    { cat ${./cluster-common.py} vol_perf_lib.py; printf '%s\n' ${lib.escapeShellArg budgetsPy}; cat vol_perf_fio.py vol_perf_main.py; } \
      | python3 -c 'import sys; compile(sys.stdin.read(), "testscript", "exec")'
    touch $out
  '';
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
    ${builtins.readFile ./python/vol_perf_lib.py}
    ${budgetsPy}
    ${builtins.readFile ./python/vol_perf_fio.py}
    ${builtins.readFile ./python/vol_perf_main.py}
  '';
}
