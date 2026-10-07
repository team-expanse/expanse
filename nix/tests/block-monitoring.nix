# monitor/prometheus + monitor/grafana: a node-exporter daemonset, a
# SINGLETON Prometheus on a replicated volume scraping every agent's
# /metrics with the cluster CA and token, and a Grafana serving the
# shipped dashboard from it. The scenario is python/block_monitoring.py.
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "block-monitoring-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    for f in ${./cluster-common.py} ${./block-common.py} ${./python/block_monitoring.py}; do
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
    expanse.agent.period = "5s";
    expanse.agent.controllerPeriod = "5s";
    expanse.agent.blocksCatalog = ../blocks;
    expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
    environment.etc."expanse/blocks-flake".source = ../blocks-flake;
    networking.firewall.allowedTCPPortRanges = [ { from = 18000; to = 18999; } ];
    # Binary-backed blocks exec upstream binaries from PATH (block-catalog.nix).
    environment.systemPackages = with pkgs; [ curl jq prometheus grafana prometheus-node-exporter ];
    virtualisation.memorySize = 3072;
    virtualisation.cores = 2;
  };
in
{
  name = "expanse-block-monitoring";

  nodes = {
    n1 = { ... }: nodeCommon 1;
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./block-common.py}
    ${builtins.readFile ./python/block_monitoring.py}
  '';
}
