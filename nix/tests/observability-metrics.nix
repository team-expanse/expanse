# PHASE-09-TASKS.md Stream A (X1): a real Prometheus-compatible /metrics
# endpoint, scraped by an actual Prometheus binary (not curled by hand),
# exporting node, resource (block/file), volume and quorum health as real
# numeric samples -- queried back through Prometheus's own HTTP API, not
# assumed from "the process started".
# The scenario is python/observability_metrics_main.py.
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "observability-metrics-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    for f in ${./cluster-common.py} ${./python/observability_metrics_main.py}; do
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
    virtualisation.memorySize = 2048;
  };
in
{
  name = "expanse-observability-metrics";

  nodes = {
    n1 = { pkgs, ... }: {
      imports = [ (nodeCommon 1) ];
      # A real prometheus binary, run ad hoc from the test script (D2's
      # bearer-token/mTLS auth needs a config prometheus's own declarative
      # NixOS module would sandbox away from at boot, before the cluster
      # CA/token even exist) -- not the systemd-managed service.
      environment.systemPackages = [ pkgs.prometheus ];
    };
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./python/observability_metrics_main.py}
  '';
}
