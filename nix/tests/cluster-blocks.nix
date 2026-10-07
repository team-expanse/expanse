# A nodeCount-node cluster running the monitoring and NFS blocks together:
# node-exporter on every node, Prometheus scraping every agent, Grafana on
# top, and an NFS export that an outside client writes to while its node is
# killed. The scenario is python/cluster_blocks.py.
{ self, nodeCount ? 12 }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "cluster-blocks-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    for f in ${./cluster-common.py} ${./block-common.py} ${./python/cluster_blocks.py}; do
      python3 -c 'import sys; compile(open(sys.argv[1]).read(), sys.argv[1], "exec")' $f
    done
    touch $out
  '';
  # The management UI always takes one pool address (share-smb.nix's note).
  vipPool = [ "192.168.1.100" "192.168.1.101" ];
  node = idx: {
    imports = [ self.nixosModules.expanse ../modules/storage-test.nix ];
    nixpkgs.overlays = [ (final: prev: { expanse = self.packages.${prev.system}.expanse; }) ];
    expanse.node.enable = true;
    expanse.agent.enable = true;
    expanse.hostId = lib.fixedWidthString 8 "0" (lib.toLower (lib.toHexString idx));
    expanse.hostname = "n${toString idx}";
    expanse.storage-test.enable = true;
    expanse.agent.period = "5s";
    expanse.agent.controllerPeriod = "5s";
    expanse.agent.blocksCatalog = ../blocks;
    expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
    environment.etc."expanse/blocks-flake".source = ../blocks-flake;
    expanse.agent.externalVIPPool = "${builtins.elemAt vipPool 0}-${builtins.elemAt vipPool 1}";
    expanse.agent.externalInterface = "eth1";
    networking.firewall.allowedTCPPorts = [ 2049 ];
    networking.firewall.allowedTCPPortRanges = [ { from = 18000; to = 18999; } ];
    # Binary-backed blocks exec upstream binaries from PATH (block-catalog.nix).
    environment.systemPackages = with pkgs; [ curl jq prometheus grafana prometheus-node-exporter nfs-ganesha ];
    virtualisation.cores = 2;
    virtualisation.memorySize = 3072;
  };
in
{
  name = "expanse-cluster-blocks-${toString nodeCount}";

  nodes = lib.genAttrs (map (i: "n${toString i}") (lib.range 1 nodeCount)) (n: { ... }: node (lib.toInt (lib.removePrefix "n" n)))
    # Outside the cluster; sorts after every nN so n1 keeps 192.168.1.1.
    // {
      zclient = { pkgs, ... }: {
        virtualisation.memorySize = 1024;
        networking.firewall.enable = false;
        boot.supportedFilesystems = [ "nfs" ];
        environment.systemPackages = [ pkgs.nfs-utils ];
      };
    };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./block-common.py}
    NODE_COUNT = ${toString nodeCount}
    ${builtins.readFile ./python/cluster_blocks.py}
  '';
}
