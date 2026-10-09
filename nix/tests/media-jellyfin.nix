# media/jellyfin: a SINGLETON Jellyfin on a DRBD-backed volume behind a VIP on 80.
# The serving node is crashed right after users are created and the survivor
# must have them. The scenario is python/media_jellyfin.py.
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "media-jellyfin-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    for f in ${./cluster-common.py} ${./block-common.py} ${./python/vol_cluster.py} ${./python/media_jellyfin.py}; do
      python3 -c 'import sys; compile(open(sys.argv[1]).read(), sys.argv[1], "exec")' $f
    done
    touch $out
  '';
  # A 5 s movie, laid out as Jellyfin's movie naming expects.
  media = pkgs.runCommand "jellyfin-test-media" { nativeBuildInputs = [ pkgs.ffmpeg-headless ]; } ''
    d="$out/movies/Test Movie (2024)"
    mkdir -p "$d"
    ffmpeg -loglevel error -f lavfi -i testsrc=duration=5:size=320x240:rate=10 \
      -f lavfi -i sine=duration=5 -c:v libx264 -c:a aac "$d/Test Movie (2024).mkv"
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
    # Jellyfin refuses to start with under 2 GiB free for its data (volume) and cache (/tmp).
    expanse.storage-test.diskSizeMB = 4096;
    virtualisation.diskSize = 4096;
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
    environment.systemPackages = [ pkgs.curl pkgs.jq pkgs.jellyfin ];
    virtualisation.memorySize = 2048;
    # Media every node reads at the same path, as from an NFS mount.
    systemd.tmpfiles.rules = [ "C /srv/media - - - - ${media}" ];
  };
in
{
  name = "expanse-media-jellyfin";

  nodes = {
    n1 = { ... }: nodeCommon 1;
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
    # External Jellyfin client, named n9 so the cluster keeps 192.168.1.1-.3.
    n9 = { ... }: {
      virtualisation.memorySize = 1024;
      networking.firewall.enable = false;
      environment.systemPackages = [ pkgs.curl ];
    };
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    client = n9
    ${builtins.readFile ./block-common.py}
    ${builtins.readFile ./python/vol_cluster.py}
    ${builtins.readFile ./python/media_jellyfin.py}
  '';
}
