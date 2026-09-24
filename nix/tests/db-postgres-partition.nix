# PHASE-05-TASKS.md R3 (split-brain prevention, D5): a real network
# partition, not a hard .crash() -- the only db-postgres VM test in this
# suite whose isolated node's agent process is never restarted, so it is
# the only one that actually exercises pgha's own held.Done()-watch
# self-fencing path. Combines db-postgres-recovery.nix's substrate
# (Stream C) with cluster-partition.nix's own nftables isolation recipe
# (§6), applied to the primary's own node instead of a plain Raft-only
# member.
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "db-postgres-partition-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    for f in ${./cluster-common.py} ${./client-common.py} ${./block-common.py} ${./python/vol_cluster.py} ${./python/db_postgres_partition.py}; do
      python3 -c 'import sys; compile(open(sys.argv[1]).read(), sys.argv[1], "exec")' $f
    done
    touch $out
  '';
  # Two addresses: the cluster's own management UI is a permanent,
  # always-on consumer of the SAME external pool (share-smb.nix's own
  # comment explains why in full) — a size-1 pool leaves nothing for the
  # block's own VIP once the UI has claimed the only address.
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
    # Tight reconcile period: pgha's own promotion-on-loss retry and its
    # own held.Done()-watch self-fencing (this test's own regression
    # target) both need to converge within the test's own budgets.
    expanse.agent.period = "5s";
    expanse.agent.controllerPeriod = "5s";
    expanse.agent.blocksCatalog = ../blocks;
    expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
    environment.etc."expanse/blocks-flake".source = ../blocks-flake;
    expanse.agent.externalVIPPool = "${builtins.elemAt vipPool 0}-${builtins.elemAt vipPool 1}";
    expanse.agent.externalInterface = "eth1";
    environment.systemPackages = [ pkgs.curl pkgs.jq pkgs.postgresql_18 ];
    networking.firewall.allowedTCPPorts = [ 5432 55432 ];
    virtualisation.memorySize = 3072;
    virtualisation.cores = 2;
  };
in
{
  name = "expanse-db-postgres-partition";

  nodes = {
    n1 = { ... }: nodeCommon 1;
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
    # The external client: a plain machine on the same LAN, no expanse.
    n9 = { ... }: {
      virtualisation.memorySize = 1024;
      networking.firewall.enable = false;
      environment.systemPackages = [ pkgs.postgresql_18 ];
    };
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./client-common.py}
    client = n9  # the external client VM
    ${builtins.readFile ./block-common.py}
    ${builtins.readFile ./python/vol_cluster.py}
    VIP_POOL = ${builtins.toJSON vipPool}
    ${builtins.readFile ./python/db_postgres_partition.py}
  '';
}
