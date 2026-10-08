# util/restic-backup: a DAEMONSET backing up a db/mariadb block's volume to an
# in-VM garage S3 endpoint from whichever node holds the volume's primary, through
# a failover, then a restore of the database from the repository. The scenario is
# python/util_restic_backup.py.
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "util-restic-backup-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    for f in ${./cluster-common.py} ${./block-common.py} ${./python/vol_cluster.py} ${./python/util_restic_backup.py}; do
      python3 -c 'import sys; compile(open(sys.argv[1]).read(), sys.argv[1], "exec")' $f
    done
    touch $out
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
    expanse.agent.period = "5s";
    expanse.agent.controllerPeriod = "5s";
    expanse.agent.blocksCatalog = ../blocks;
    expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
    environment.etc."expanse/blocks-flake".source = ../blocks-flake;
    expanse.agent.externalVIPPool = "${builtins.elemAt vipPool 0}-${builtins.elemAt vipPool 1}";
    expanse.agent.externalInterface = "eth1";
    # The NixOS firewall is on; the agent's own nftables ruleset is opt-in.
    networking.firewall.allowedTCPPorts = [ 3306 ];
    # Binary-backed blocks exec upstream binaries from PATH (block-catalog.nix).
    environment.systemPackages = [ pkgs.curl pkgs.jq pkgs.mariadb pkgs.restic ];
    virtualisation.memorySize = 1536;
  };
in
{
  name = "expanse-util-restic-backup";

  nodes = {
    n1 = { ... }: nodeCommon 1;
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
    # The S3 endpoint and database client, named n9 so the cluster keeps 192.168.1.1-.3.
    n9 = { ... }: {
      virtualisation.memorySize = 1024;
      networking.firewall.enable = false;
      environment.systemPackages = [ pkgs.mariadb pkgs.restic ];
      services.garage = {
        enable = true;
        package = pkgs.garage;
        settings = {
          replication_factor = 1;
          rpc_bind_addr = "[::]:3901";
          rpc_public_addr = "127.0.0.1:3901";
          # Test-only secret, fixed so the test is reproducible.
          rpc_secret = "b0aa753b23ae60c8c7baf46d0fb34cdf43806570f977dd9d81473072c72de5f0";
          s3_api = {
            s3_region = "garage";
            api_bind_addr = "[::]:3900";
            root_domain = ".s3.garage.localhost";
          };
        };
      };
    };
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    client = n9
    ${builtins.readFile ./block-common.py}
    ${builtins.readFile ./python/vol_cluster.py}
    ${builtins.readFile ./python/util_restic_backup.py}
  '';
}
