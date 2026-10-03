# Cluster administration with every agent running: a follower mints a
# join token, a 4th node joins, leadership moves on request, and nodes
# are removed with `ctl node remove` and `cluster leave`.
{ self }:
{ pkgs, lib, ... }:
let
  node = { hostname, hostId }: { ... }: {
    imports = [ self.nixosModules.expanse ];
    nixpkgs.overlays = [
      (final: prev: { expanse = self.packages.${prev.system}.expanse; })
    ];
    expanse.node.enable = true;
    expanse.agent.enable = true;
    expanse.hostId = hostId;
    expanse.hostname = hostname;
    virtualisation.memorySize = 1536;
  };
in
{
  name = "expanse-cluster-admin";

  nodes = {
    n1 = node { hostname = "n1"; hostId = "00000001"; };
    n2 = node { hostname = "n2"; hostId = "00000002"; };
    n3 = node { hostname = "n3"; hostId = "00000003"; };
    n4 = node { hostname = "n4"; hostId = "00000004"; };
  };

  testScript = ''
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./python/cluster_admin.py}
  '';
}
