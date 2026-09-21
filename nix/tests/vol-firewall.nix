# Phase 1 B8: DRBD replication with the agent's nftables ruleset enforced (the NixOS firewall
# is off), three storage nodes and an off-mesh client. The volume replicates over the mesh and
# its port is dropped from every other interface.
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "vol-firewall-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    for f in ${./cluster-common.py} ${./python/vol_cluster.py} ${./python/vol_firewall_main.py}; do
      python3 -c 'import sys; compile(open(sys.argv[1]).read(), sys.argv[1], "exec")' $f
    done
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
    expanse.node.enable = true;
    expanse.agent.enable = true;
    expanse.hostId = "0000000${toString idx}";
    expanse.hostname = "n${toString idx}";
    expanse.storage-test.enable = true;
    virtualisation.memorySize = 1536;
    # The agent is the only nftables owner: the NixOS firewall would flush its table.
    expanse.agent.firewall = true;
    networking.nftables.enable = lib.mkForce false;
    networking.firewall.enable = lib.mkForce false;
    environment.systemPackages = [ pkgs.nftables ];
  };
in
{
  name = "expanse-vol-firewall";

  nodes = {
    n1 = { ... }: nodeCommon 1;
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
    # Sorts after n3, so the driver gives it eth1 192.168.1.4: on the LAN, never on the mesh.
    n9 = { ... }: {
      virtualisation.memorySize = 512;
      networking.firewall.enable = false;
    };
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./python/vol_cluster.py}
    ${builtins.readFile ./python/vol_firewall_main.py}
  '';
}
