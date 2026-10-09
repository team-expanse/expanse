# PHASE-09-TASKS.md Stream E (X5, the release blocker): ROADMAP.md's own
# exit line for this phase, verbatim -- a node failure raises an alert
# and is visible in the UI and dashboards within 30s, measured end to
# end with a real killed node and a real stopwatch. Combines Stream A's
# real Prometheus scrape, Stream B's real alert rules (including the
# ExpanseNodeUnreachable rule this stream itself added), Stream C's real
# Grafana, and Stream D's real /health and /cluster UI pages into one
# scenario, all pointed at one real 3-node cluster.
# The scenario is python/observability_vertical_slice_main.py.
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "observability-vertical-slice-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    for f in ${./cluster-common.py} ${./python/observability_vertical_slice_main.py}; do
      python3 -c 'import sys; compile(open(sys.argv[1]).read(), sys.argv[1], "exec")' $f
    done
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
    # Grafana is heavier than prometheus/promtool alone (observability-
    # grafana.nix's own precedent) -- applied to all three nodes here
    # too, simplest is uniform rather than a second scalar override.
    virtualisation.memorySize = 3072;
  };
in
{
  name = "expanse-observability-vertical-slice";

  # Every node also gets curl-cookie-jar.nix (curl/curl#23261).
  nodes = lib.mapAttrs (_: node: { imports = [ node ./curl-cookie-jar.nix ]; }) {
    n1 = { pkgs, ... }: {
      imports = [ (nodeCommon 1) ];
      # Real prometheus and grafana binaries, run ad hoc -- same
      # reasoning as observability-metrics.nix/observability-grafana.nix:
      # the cluster CA/token don't exist until after cluster init.
      environment.systemPackages = [ pkgs.prometheus pkgs.grafana (pkgs.callPackage ../grafana-plugins.nix { }) ];
      # Extra headroom for Grafana's own background work (search
      # indexing, ngalert's scheduler) alongside this node's raft agent
      # and prometheus, all sharing one node -- mirrors observability-
      # grafana.nix.
      virtualisation.cores = 2;
    };
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
  };

  testScript = ''
    RULES_FILE = "${../../deploy/prometheus/expanse-alerts.rules.yml}"
    GRAFANA_HOMEPATH = "${pkgs.grafana}/share/grafana"
    DATASOURCE_YAML = "${../../deploy/grafana/provisioning/datasources/datasource.yaml}"
    DASHBOARDS_YAML = "${../../deploy/grafana/provisioning/dashboards/dashboards.yaml}"
    DASHBOARD_JSON = "${../../deploy/grafana/dashboards/expanse-cluster-health.json}"
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./python/observability_vertical_slice_main.py}
  '';
}
