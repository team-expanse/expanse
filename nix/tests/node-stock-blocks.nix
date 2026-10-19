# 1.11.1: a block on a node configured as the installer leaves it (firewall on, no runtimes
# pre-installed). The scenario is python/node_stock_blocks.py; n9 is the external client.
{ self }:
{ pkgs, lib, ... }:
let
  shim = "n2 = n3 = n1\n";
  lint = pkgs.runCommand "node-stock-blocks-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    { cat ${./cluster-common.py} ${./block-common.py}; printf '%s' ${lib.escapeShellArg shim}; cat ${./python/vol_cluster.py} ${./python/single_node_common.py} ${./python/node_stock_blocks.py}; } \
      | python3 -c 'import sys; compile(sys.stdin.read(), "testscript", "exec")'
    touch $out
  '';
in
{
  name = "expanse-node-stock-blocks";

  nodes = {
    n1 = { ... }: {
      imports = [ self.nixosModules.expanse ];
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
      expanse.agent.externalVIPPool = "192.168.1.100-192.168.1.103";
      expanse.agent.externalInterface = "eth1";
      # A runtime the agent must build (offline): the same build-user switch as a real one.
      environment.etc."expanse/block-runtimes.nix".text = lib.mkForce ''
        let sh = builtins.storePath "${pkgs.busybox}"; nginx = builtins.storePath "${pkgs.nginx}"; in
        builtins.mapAttrs (_: t: derivation {
          name = "expanse-runtime-web-nginx";
          system = "${pkgs.stdenv.hostPlatform.system}";
          builder = "''${sh}/bin/ln";
          args = [ "-s" nginx (placeholder "out") ];
        }) { "web/nginx" = null; }
      '';
      nix.settings.substituters = lib.mkForce [ ];
      environment.systemPackages = [ pkgs.curl ];
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
    ${builtins.readFile ./python/node_stock_blocks.py}
  '';
}
