# A node removed from the cluster while it runs a block replica stops that
# replica and says it was removed, rather than carrying on unaware.
{ self }:
{ pkgs, lib, ... }:
let
  mkNode = name: hostId: { ... }: {
    imports = [ self.nixosModules.expanse ];
    nixpkgs.overlays = [
      (final: prev: { expanse = self.packages.${prev.system}.expanse; })
    ];
    expanse.node.enable = true;
    expanse.agent.enable = true;
    expanse.hostId = hostId;
    expanse.hostname = name;
    expanse.agent.period = "5s";
    expanse.agent.blocksCatalog = ../blocks;
    expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
    environment.etc."expanse/blocks-flake".source = ../blocks-flake;
    environment.systemPackages = with pkgs; [ curl ];
    virtualisation.memorySize = 2048;
  };
in
{
  name = "expanse-node-removed";

  nodes = {
    n1 = mkNode "n1" "00000001";
    n2 = mkNode "n2" "00000002";
    n3 = mkNode "n3" "00000003";
  };

  testScript = ''
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./block-common.py}
    ${builtins.readFile ./python/node_removed.py}
  '';
}
