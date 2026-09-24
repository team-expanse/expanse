# PHASE-05-TASKS.md Stream C: after a db/postgres primary is lost and a
# replica promotes, the promoted primary is provably uncorrupted
# (amcheck's own live index check plus a brief pg_checksums pass, X3),
# and the old primary's node, once it rejoins, comes back as a fresh
# streaming replica of the new primary rather than a permanently
# diverged standalone primary or requiring a manual pg_rewind (X4).
# Combines db-postgres-failover.nix's substrate (Stream B) with a
# rejoin phase share-smb-failover.nix's own crash()-then-start() pattern
# does not need (that block's SINGLETON volume reschedules; this one's
# per-replica volume is pinned to its own node, D3).
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "db-postgres-recovery-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    for f in ${./cluster-common.py} ${./client-common.py} ${./block-common.py} ${./python/vol_cluster.py} ${./python/db_postgres_recovery.py}; do
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
    # Tight reconcile period: pgha's own promotion-on-loss retry, its
    # demotion-on-reclaim-loss (Stream C), and the LB pool's PrimaryOnly
    # routing all need to converge within the test's own budgets.
    expanse.agent.period = "5s";
    expanse.agent.controllerPeriod = "5s";
    expanse.agent.blocksCatalog = ../blocks;
    expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
    environment.etc."expanse/blocks-flake".source = ../blocks-flake;
    expanse.agent.externalVIPPool = "${builtins.elemAt vipPool 0}-${builtins.elemAt vipPool 1}";
    expanse.agent.externalInterface = "eth1";
    # util-linux: setpriv, so the test can run pg_checksums/pg_ctl as
    # pgdata's own owning uid (postgres's frontend tools refuse root).
    environment.systemPackages = [ pkgs.curl pkgs.jq pkgs.postgresql_18 pkgs.util-linux ];
    networking.firewall.allowedTCPPorts = [ 5432 55432 ];
    # More headroom than X1/X2's tests need: --data-checksums (X3's own
    # prerequisite) makes initdb heavier, and this test's own setup
    # (pgbench -i plus extra table writes) adds more concurrent work
    # during the same 3-way bootstrap race than X1/X2 exercise at that
    # point -- a replica's own pg_basebackup was observed stalling for
    # the rest of the run under the default budget (diagnosed as
    # resource contention, not a hang: pg_basebackup completes in well
    # under a second against the same primary in isolation).
    virtualisation.memorySize = 3072;
    virtualisation.cores = 2;
  };
in
{
  name = "expanse-db-postgres-recovery";

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
    ${builtins.readFile ./python/db_postgres_recovery.py}
  '';
}
