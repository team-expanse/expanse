# Phase 2, D1: volume handlers over the web UI -- create, watch
# placement land live over SSE, resize, snapshot, and inspect the
# replica table, all through HTTP forms/SSE, none through `expanse
# ctl`. Builds on ui-auth.nix's login flow; this is Stream D's own
# acceptance gate (X6).
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "ui-volumes-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    cd ${./python}
    cat ${./cluster-common.py} ui_volumes.py \
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
    environment.systemPackages = with pkgs; [ curl jq ];
    expanse.node.enable = true;
    expanse.agent.enable = true;
    expanse.hostId = "0000000${toString idx}";
    expanse.hostname = "n${toString idx}";
    expanse.storage-test.enable = true;
    virtualisation.memorySize = 2048;
  };
in
{
  name = "expanse-ui-volumes";

  nodes = {
    n1 = { ... }: nodeCommon 1;
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./python/ui_volumes.py}
  '';
}
