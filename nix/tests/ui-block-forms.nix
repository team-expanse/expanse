# Phase 14 D: a block deployed and edited through the web UI's forms alone, on a one-node
# cluster. The scenario is python/ui_block_forms.py; n9 is the external client.
{ self }:
{ pkgs, lib, ... }:
let
  # vol_cluster.py's NODES names n1..n3; this test has n1 only (NODES is unused here).
  shim = "n2 = n3 = n1\n";
  lint = pkgs.runCommand "ui-block-forms-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    { cat ${./cluster-common.py} ${./block-common.py}; printf '%s' ${lib.escapeShellArg shim}; cat ${./python/vol_cluster.py} ${./python/single_node_common.py} ${./python/ui_forms_common.py} ${./python/ui_block_forms.py}; } \
      | python3 -c 'import sys; compile(sys.stdin.read(), "testscript", "exec")'
    touch $out
  '';
in
{
  name = "expanse-ui-block-forms";

  nodes = {
    n1 = { ... }: {
      imports = [
        self.nixosModules.expanse
        (import ./lib/block-runtimes.nix [ "web/nginx" ])
        ./curl-cookie-jar.nix
      ];
      nixpkgs.overlays = [
        (final: prev: { expanse = self.packages.${prev.system}.expanse; })
      ];
      expanse.node.enable = true;
      expanse.agent.enable = true;
      expanse.hostId = "00000001";
      expanse.hostname = "n1";
      expanse.agent.raftAdvertise = "192.168.1.1:7444";
      expanse.agent.period = "5s";
      expanse.agent.controllerPeriod = "5s";
      expanse.agent.blocksCatalog = ../blocks;
      expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
      environment.etc."expanse/blocks-flake".source = ../blocks-flake;
      # Two addresses: the web UI's own VIP takes one at agent start.
      expanse.agent.externalVIPPool = "192.168.1.100-192.168.1.103"; # the UI, site and r1
      expanse.agent.externalInterface = "eth1";
      environment.systemPackages = with pkgs; [ curl ];
      virtualisation.memorySize = 2048;
    };
    n9 = { ... }: {
      virtualisation.memorySize = 512;
      networking.firewall.enable = false;
      environment.systemPackages = [ pkgs.curl ];
    };
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./block-common.py}
    ${shim}
    ${builtins.readFile ./python/vol_cluster.py}
    ${builtins.readFile ./python/single_node_common.py}
    ${builtins.readFile ./python/ui_forms_common.py}
    ${builtins.readFile ./python/ui_block_forms.py}
  '';
}
