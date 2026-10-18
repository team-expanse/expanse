# Phase 14 B: passwords given or generated in the deploy form are sealed in the cluster's secret
# store and reach the block only as systemd credentials. The scenario is python/ui_secrets.py.
{ self }:
{ pkgs, lib, ... }:
let
  # vol_cluster.py's NODES names n1..n3; this test has n1 only (NODES is unused here).
  shim = "n2 = n3 = n1\n";
  lint = pkgs.runCommand "ui-secrets-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    { cat ${./cluster-common.py} ${./block-common.py}; printf '%s' ${lib.escapeShellArg shim}; cat ${./python/vol_cluster.py} ${./python/single_node_common.py} ${./python/ui_forms_common.py} ${./python/ui_secrets.py}; } \
      | python3 -c 'import sys; compile(sys.stdin.read(), "testscript", "exec")'
    touch $out
  '';
in
{
  name = "expanse-ui-secrets";

  nodes.n1 = { ... }: {
    imports = [
      self.nixosModules.expanse
      (import ./lib/block-runtimes.nix [ "db/postgres" ])
      ../modules/storage-test.nix
      ./curl-cookie-jar.nix
    ];
    nixpkgs.overlays = [
      (final: prev: { expanse = self.packages.${prev.system}.expanse; })
    ];
    expanse.node.enable = true;
    expanse.agent.enable = true;
    expanse.hostId = "00000001";
    expanse.hostname = "n1";
    expanse.storage-test.enable = true;
    expanse.agent.raftAdvertise = "192.168.1.1:7444";
    expanse.agent.period = "5s";
    expanse.agent.controllerPeriod = "5s";
    expanse.agent.blocksCatalog = ../blocks;
    expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
    environment.etc."expanse/blocks-flake".source = ../blocks-flake;
    environment.systemPackages = with pkgs; [ curl ];
    virtualisation.memorySize = 2048;
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./block-common.py}
    ${shim}
    ${builtins.readFile ./python/vol_cluster.py}
    ${builtins.readFile ./python/single_node_common.py}
    ${builtins.readFile ./python/ui_forms_common.py}
    ${builtins.readFile ./python/ui_secrets.py}
  '';
}
