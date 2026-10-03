# §8 block-daemonset: deploy node-exporter-style daemonset (V6: one
# placement per eligible node — Ready, non-witness); assert 1 per node;
# add a node → it gets one automatically; cordon a node → every replica
# STAYS past the unreachable grace; drain → they move off; uncordon ends it.
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
    environment.systemPackages = with pkgs; [ openssl curl ];
    virtualisation.memorySize = 2048;
    # Replica endpoints are curled across nodes.
    networking.firewall.allowedTCPPortRanges = [{ from = 18000; to = 18999; }];
    # Tight reconcile period.
    expanse.agent.period = "5s";
    expanse.agent.controllerPeriod = "5s";
    expanse.agent.blocksCatalog = ../blocks;
    expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
    environment.etc."expanse/blocks-flake".source = ../blocks-flake;
  };
in
{
  name = "expanse-block-daemonset";

  nodes = {
    n1 = mkNode "n1" "00000001";
    n2 = mkNode "n2" "00000002";
    n3 = mkNode "n3" "00000003";
    # The late joiner (starts idle; joins via the CLI after the first
    # assertions) must also be reachable for its new daemonset replica.
    n4 = mkNode "n4" "00000004";
  };

  testScript = ''
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./block-common.py}

    ${builtins.readFile ./python/block_daemonset.py}
  '';
}
