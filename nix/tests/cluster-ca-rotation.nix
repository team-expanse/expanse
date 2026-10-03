# Phase 10 X2: a running 3-node cluster rotates to a fresh root CA with
# zero downtime (quorum and reads/writes keep working throughout, no
# node evicted), every node's renewal loop reissues its own cert onto
# the new CA with no listener restart, and the old CA is retired at the
# end. `expanse cluster ca rotate/status/complete` go through the
# running agents, so no daemon stops at any point. `renewalPeriod` is
# set short so the test doesn't wait out a real 30-day cert lifetime.
{ self }:
{ pkgs, lib, ... }:
let
  expanse = self.packages.${pkgs.system}.expanse;
  node = { hostname, hostId }: { ... }: {
    imports = [ self.nixosModules.expanse ];
    nixpkgs.overlays = [
      (final: prev: { expanse = self.packages.${prev.system}.expanse; })
    ];
    expanse.node.enable = true;
    expanse.agent.enable = true;
    expanse.agent.renewalPeriod = "3s";
    expanse.hostId = hostId;
    expanse.hostname = hostname;
    virtualisation.memorySize = 1536;
  };
in
{
  name = "expanse-cluster-ca-rotation";

  nodes = {
    n1 = node { hostname = "n1"; hostId = "00000001"; };
    n2 = node { hostname = "n2"; hostId = "00000002"; };
    n3 = node { hostname = "n3"; hostId = "00000003"; };
  };

  testScript = ''
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./python/cluster_ca_rotation.py}
  '';
}
