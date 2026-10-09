# Phase 2, C1: block handlers over the web UI -- deploy, watch it reach
# RUNNING via SSE, scale, delete, and tail live logs, all through HTTP
# forms/SSE, none through `expanse ctl`. Builds on ui-auth.nix's login
# flow; this is Stream C's own acceptance gate (X1, X5).
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "ui-blocks-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    cd ${./python}
    cat ${./cluster-common.py} ${./block-common.py} ui_blocks.py \
      | python3 -c 'import sys; compile(sys.stdin.read(), "testscript", "exec")'
    touch $out
  '';
  nodeCommon = idx: {
    imports = [ self.nixosModules.expanse ];
    nixpkgs.overlays = [
      (final: prev: { expanse = self.packages.${prev.system}.expanse; })
    ];
    # nginx: the blocks-flake's web/nginx stub execs the bare "nginx"
    # command expecting it on PATH (net-vip-basic.nix's own nginx
    # deploy relies on the identical workaround).
    environment.systemPackages = with pkgs; [ curl jq nginx ];
    expanse.node.enable = true;
    expanse.agent.enable = true;
    expanse.hostId = "0000000${toString idx}";
    expanse.hostname = "n${toString idx}";
    virtualisation.memorySize = 2048;
    # Tight reconcile period (matches block-deploy.nix): the UI's SSE
    # events subtest polls a real placement pipeline, not a stub.
    # controllerPeriod also tightened (block-deploy.nix leaves it at its
    # 30s default and gets away with it on Wake()-triggered timing luck;
    # SSE-observed promotion here needs it deterministic, not lucky --
    # RuntimePass's health-status reads aren't among the watched
    # prefixes that trigger Wake(), only the interval backstop is).
    expanse.agent.period = "5s";
    expanse.agent.controllerPeriod = "5s";
    expanse.agent.blocksCatalog = ../blocks;
    expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
    environment.etc."expanse/blocks-flake".source = ../blocks-flake;
  };
in
{
  name = "expanse-ui-blocks";

  # Every node also gets curl-cookie-jar.nix (curl/curl#23261).
  nodes = lib.mapAttrs (_: node: { imports = [ node ./curl-cookie-jar.nix ]; }) {
    n1 = { ... }: nodeCommon 1;
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./block-common.py}
    ${builtins.readFile ./python/ui_blocks.py}
  '';
}
