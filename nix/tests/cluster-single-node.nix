# Phase 12 C1: a one-node cluster serves a default volume, block storage and a VIP, and
# survives a reboot. The scenario is python/single_node_main.py; n9 is the external client.
{ self }:
{ pkgs, lib, ... }:
let
  # vol_cluster.py's NODES names n1..n3; this test has n1 only (NODES is unused here).
  shim = "n2 = n3 = n1\n";
  lint = pkgs.runCommand "cluster-single-node-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    { cat ${./cluster-common.py} ${./block-common.py}; printf '%s' ${lib.escapeShellArg shim}; cat ${./python/vol_cluster.py} ${./python/single_node_common.py} ${./python/single_node_main.py}; } \
      | python3 -c 'import sys; compile(sys.stdin.read(), "testscript", "exec")'
    touch $out
  '';
in
{
  name = "expanse-cluster-single-node";

  nodes = {
    n1 = { ... }: {
      imports = [
        self.nixosModules.expanse
        ../modules/storage-test.nix
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
      # Two addresses: the web UI's own VIP takes one at agent start.
      expanse.agent.externalVIPPool = "192.168.1.100-192.168.1.101";
      expanse.agent.externalInterface = "eth1";
      networking.firewall.allowedTCPPorts = [ 80 8080 18090 ];
      environment.systemPackages = with pkgs; [ curl nginx ];
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
    ${builtins.readFile ./python/single_node_main.py}
  '';
}
