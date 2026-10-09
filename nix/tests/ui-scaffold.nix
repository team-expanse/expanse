# Phase 2, A1: expanse agent serves the web UI's placeholder page over
# TLS signed by the cluster CA on every cluster node (config.PortUI).
# Auth (A2) and real content (Streams B-D) land later; this is the
# scaffold's own acceptance test.
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "ui-scaffold-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    cd ${./python}
    cat ${./cluster-common.py} ui_scaffold.py \
      | python3 -c 'import sys; compile(sys.stdin.read(), "testscript", "exec")'
    touch $out
  '';
  nodeCommon = idx: {
    imports = [ self.nixosModules.expanse ];
    nixpkgs.overlays = [
      (final: prev: { expanse = self.packages.${prev.system}.expanse; })
    ];
    environment.systemPackages = [ pkgs.curl ];
    expanse.node.enable = true;
    expanse.agent.enable = true;
    expanse.hostId = "0000000${toString idx}";
    expanse.hostname = "n${toString idx}";
    virtualisation.memorySize = 1536;
  };
in
{
  name = "expanse-ui-scaffold";

  # Every node also gets curl-cookie-jar.nix (curl/curl#23261).
  nodes = lib.mapAttrs (_: node: { imports = [ node ./curl-cookie-jar.nix ]; }) {
    n1 = { ... }: nodeCommon 1;
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./python/ui_scaffold.py}
  '';
}
