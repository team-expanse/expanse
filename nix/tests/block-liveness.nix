# block-liveness: a replica whose liveness probe fails (its process frozen,
# so the unit stays active) is restarted on the same node by its agent; once
# its restarts run out it moves to another node, and failing there too stops it.
{ self }:
{ pkgs, lib, ... }:
let
  node = id: { ... }: {
    imports = [ self.nixosModules.expanse ];
    nixpkgs.overlays = [
      (final: prev: { expanse = self.packages.${prev.system}.expanse; })
    ];
    expanse.node.enable = true;
    expanse.agent.enable = true;
    expanse.hostId = "0000000${toString id}";
    expanse.hostname = "n${toString id}";
    virtualisation.memorySize = 2048;
    expanse.agent.period = "5s";
    expanse.agent.controllerPeriod = "5s";
    expanse.agent.livenessMaxRestarts = 1;
    expanse.agent.blocksCatalog = ../blocks;
    expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
    environment.etc."expanse/blocks-flake".source = ../blocks-flake;
    networking.firewall.allowedTCPPorts = [ 8080 ];
    environment.systemPackages = with pkgs; [ curl jq ];
  };
in
{
  name = "expanse-block-liveness";

  nodes = {
    n1 = node 1;
    n2 = node 2;
    n3 = node 3;
  };

  testScript = ''
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./block-common.py}
    ${builtins.readFile ./python/block_liveness.py}
  '';
}
