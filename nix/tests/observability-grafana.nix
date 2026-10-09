# PHASE-09-TASKS.md Stream C (X3): the shipped Grafana provisioning
# (deploy/grafana/) and dashboard (deploy/grafana/dashboards/
# expanse-cluster-health.json) loaded by a real Grafana pointed at the
# real Prometheus from Stream A, with the dashboard's own panel queries
# proven to return real data back through Grafana's own HTTP API --
# not just "the dashboard JSON exists".
# The scenario is python/observability_grafana_main.py.
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "observability-grafana-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    for f in ${./cluster-common.py} ${./python/observability_grafana_main.py}; do
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
    # Grafana is heavier than prometheus/promtool alone -- more headroom
    # than the other observability streams' 2048.
    virtualisation.memorySize = 3072;
  };
in
{
  name = "expanse-observability-grafana";

  nodes = {
    n1 = { pkgs, ... }: {
      imports = [ (nodeCommon 1) ];
      # Real prometheus and grafana binaries, run ad hoc from the test
      # script -- same reasoning as observability-metrics.nix: the
      # cluster CA/token don't exist until after cluster init, and D2's
      # stance is "bring your own", not a bundled systemd service.
      environment.systemPackages = [ pkgs.prometheus pkgs.grafana (pkgs.callPackage ../grafana-plugins.nix { }) ];
      # Extra headroom for Grafana's own background work (search
      # indexing, ngalert's scheduler) alongside this node's raft agent
      # and prometheus, all sharing one node.
      virtualisation.cores = 2;
    };
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
  };

  testScript = ''
    GRAFANA_HOMEPATH = "${pkgs.grafana}/share/grafana"
    DATASOURCE_YAML = "${../../deploy/grafana/provisioning/datasources/datasource.yaml}"
    DASHBOARDS_YAML = "${../../deploy/grafana/provisioning/dashboards/dashboards.yaml}"
    DASHBOARD_JSON = "${../../deploy/grafana/dashboards/expanse-cluster-health.json}"
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./python/observability_grafana_main.py}
  '';
}
