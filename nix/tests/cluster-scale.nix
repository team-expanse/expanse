# A nodeCount-node cluster, each VM pinned to its own physical cores: forming, mesh, block
# spread, raft from every node, a partition, drain and removal, and a full agent restart.
# Checks that things work at scale, not how fast. On request only; the scenario is
# python/cluster_scale.py.
{ self, nodeCount ? 12, coresPerVM ? 1 }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "cluster-scale-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    cd ${./python}
    python3 vm_pin_test.py
    for f in ${./cluster-common.py} ${./block-common.py} ${./client-common.py} vm_pin.py cluster_scale.py; do
      python3 -c 'import sys; compile(open(sys.argv[1]).read(), sys.argv[1], "exec")' $f
    done
    touch $out
  '';
  node = idx: {
    imports = [ self.nixosModules.expanse ];
    nixpkgs.overlays = [ (final: prev: { expanse = self.packages.${prev.system}.expanse; }) ];
    expanse.node.enable = true;
    expanse.agent.enable = true;
    expanse.hostId = lib.fixedWidthString 8 "0" (lib.toLower (lib.toHexString idx));
    expanse.hostname = "n${toString idx}";
    expanse.agent.period = "5s";
    expanse.agent.controllerPeriod = "5s";
    expanse.agent.blocksCatalog = ../blocks;
    expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
    environment.etc."expanse/blocks-flake".source = ../blocks-flake;
    networking.firewall.allowedUDPPorts = [ 51820 ];
    networking.firewall.allowedTCPPortRanges = [ { from = 18000; to = 18999; } ];
    environment.systemPackages = with pkgs; [ openssl curl wireguard-tools ];
    virtualisation.cores = 2;
    virtualisation.memorySize = 2048;
  };
in
{
  name = "expanse-cluster-scale-${toString nodeCount}";

  nodes = lib.genAttrs (map (i: "n${toString i}") (lib.range 1 nodeCount)) (n: { ... }: node (lib.toInt (lib.removePrefix "n" n)));

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./block-common.py}
    ${builtins.readFile ./client-common.py}
    ${builtins.readFile ./python/vm_pin.py}
    NODE_COUNT = ${toString nodeCount}
    CORES_PER_VM = ${toString coresPerVM}
    ${builtins.readFile ./python/cluster_scale.py}
  '';
}
