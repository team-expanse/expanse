# Phase 2, B1: the cluster overview page over the web UI -- nodes,
# quorum, leader and generation, pushed live over SSE, never polled.
# Builds on ui-auth.nix's login flow; this is Stream B's own
# acceptance gate (X4): join/fail a node and a generation change are
# both observed live.
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "ui-cluster-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    cd ${./python}
    cat ${./cluster-common.py} ui_cluster.py \
      | python3 -c 'import sys; compile(sys.stdin.read(), "testscript", "exec")'
    touch $out
  '';
  nodeCommon = idx: {
    imports = [ self.nixosModules.expanse ];
    nixpkgs.overlays = [
      (final: prev: { expanse = self.packages.${prev.system}.expanse; })
    ];
    environment.systemPackages = with pkgs; [ curl jq ];
    expanse.node.enable = true;
    expanse.agent.enable = true;
    expanse.hostId = "0000000${toString idx}";
    expanse.hostname = "n${toString idx}";
    virtualisation.memorySize = 2048;
  };
in
{
  name = "expanse-ui-cluster";

  nodes = {
    n1 = { ... }: nodeCommon 1;
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./python/ui_cluster.py}
  '';
}
