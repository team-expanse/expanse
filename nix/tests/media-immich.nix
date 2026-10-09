# media/immich: a SINGLETON Immich on a DRBD-backed volume behind a VIP on 80.
# The serving node is crashed right after photos are uploaded and the survivor
# must have them and their queued jobs. The scenario is python/media_immich.py.
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "media-immich-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    for f in ${./cluster-common.py} ${./block-common.py} ${./python/vol_cluster.py} ${./python/immich_client.py} ${./python/media_immich.py}; do
      python3 -c 'import sys; compile(open(sys.argv[1]).read(), sys.argv[1], "exec")' $f
    done
    touch $out
  '';
  # The REST client the scenario drives Immich with, as its apps do.
  immich = pkgs.writeShellScriptBin "immich" ''
    exec ${pkgs.python3.withPackages (p: [ p.requests ])}/bin/python3 ${./python/immich_client.py} "$@"
  '';
  # The management UI always takes one pool address (share-smb.nix's note).
  vipPool = [ "192.168.1.100" "192.168.1.101" ];
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
    # Immich imports its reverse-geocoding data into Postgres on first start.
    expanse.storage-test.diskSizeMB = 4096;
    expanse.agent.period = "5s";
    expanse.agent.controllerPeriod = "5s";
    expanse.agent.blocksCatalog = ../blocks;
    expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
    environment.etc."expanse/blocks-flake".source = ../blocks-flake;
    expanse.agent.externalVIPPool = "${builtins.elemAt vipPool 0}-${builtins.elemAt vipPool 1}";
    expanse.agent.externalInterface = "eth1";
    # The NixOS firewall is on; the agent's own nftables ruleset is opt-in.
    networking.firewall.allowedTCPPorts = [ 80 ];
    # Binary-backed blocks exec upstream binaries from PATH (block-catalog.nix).
    environment.systemPackages = [
      pkgs.curl
      pkgs.jq
      (pkgs.postgresql_18.withPackages (ps: [ ps.pgvector ps.vectorchord ]))
      pkgs.redis
      pkgs.immich
    ];
    virtualisation.memorySize = 3072;
  };
in
{
  name = "expanse-media-immich";

  nodes = {
    n1 = { ... }: nodeCommon 1;
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
    # External client, named n9 so the cluster keeps 192.168.1.1-.3.
    n9 = { ... }: {
      virtualisation.memorySize = 1024;
      environment.systemPackages = [ pkgs.curl immich ];
    };
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    client = n9
    ${builtins.readFile ./block-common.py}
    ${builtins.readFile ./python/vol_cluster.py}
    ${builtins.readFile ./python/media_immich.py}
  '';
}
