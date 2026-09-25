# PHASE-09-TASKS.md Stream B (X2): the shipped alert rules
# (deploy/prometheus/expanse-alerts.rules.yml) evaluated by a real
# Prometheus against a real 3-node cluster, observed to actually fire
# under genuinely degraded fixtures -- not just "the rule file parses".
# The rule PromQL itself is also unit-tested directly by promtool, see
# deploy/prometheus/expanse-alerts.rules_test.yml.
# The scenario is python/observability_alerts_main.py.
{ self }:
{ pkgs, lib, ... }:
let
  rulesFile = ../../deploy/prometheus/expanse-alerts.rules.yml;
  lint = pkgs.runCommand "observability-alerts-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    for f in ${./cluster-common.py} ${./python/observability_alerts_main.py}; do
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
    virtualisation.memorySize = 2048;
  };
in
{
  name = "expanse-observability-alerts";

  nodes = {
    n1 = { pkgs, ... }: {
      imports = [ (nodeCommon 1) ];
      # A real prometheus binary, run ad hoc (D2's mTLS/bearer-token
      # scrape config needs the cluster CA/token, which don't exist
      # until after cluster init -- see observability-metrics.nix).
      environment.systemPackages = [ pkgs.prometheus ];
    };
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
  };

  testScript = ''
    RULES_FILE = "${rulesFile}"
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./python/observability_alerts_main.py}
  '';
}
