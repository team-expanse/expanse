# PHASE-05-TASKS.md Stream D (X5, non-blocking): the vertical slice --
# pgbench sustains continuous load and a precisely-tracked ack writer
# runs through a hard primary kill (X2's own mechanism and correctness
# oracle), then amcheck and pg_checksums confirm zero corruption on the
# SAME promoted primary from the SAME run (X3), and the old primary's
# node rejoins as a genuine streaming replica (X4). Combines
# db-postgres-failover.nix's (Stream B) and db-postgres-recovery.nix's
# (Stream C) own substrates into one continuous scenario rather than
# two isolated ones.
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "db-postgres-vertical-slice-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    for f in ${./cluster-common.py} ${./client-common.py} ${./block-common.py} ${./python/vol_cluster.py} ${./python/db_postgres_vertical_slice.py}; do
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
    # util-linux: setpriv, so the test can run pg_checksums as pgdata's
    # own owning uid (postgres's frontend tools refuse root).
    environment.systemPackages = [ pkgs.curl pkgs.jq pkgs.postgresql_18 pkgs.util-linux ];
    networking.firewall.allowedTCPPorts = [ 5432 55432 ];
    # db-postgres-recovery.nix's own proven budget: --data-checksums (X3)
    # makes initdb heavier, and this test's own setup (pgbench -i plus
    # the ack-tracked/recovery-check tables) adds concurrent work during
    # the 3-way bootstrap race beyond X1/X2 alone. Combining Stream B's
    # sustained pgbench+ack-writer load with Stream C's own checks on the
    # SAME run surfaced two real bugs during development, both fixed in
    # code, not by throwing more VM resources at the symptom: (1) the LB
    # pool's PrimaryOnly routing treated winning the primary lease alone
    # as sufficient proof of being primary, routing writes to a node
    # whose own promotion attempt had failed and been abandoned
    # (internal/proxy's own confirmedPrimaryPrefix); (2) PrimaryOnly's
    # candidate set is never more than one backend by design, so a
    # single transient dial failure against it closed the client's
    # connection outright with nothing left to retry (internal/proxy's
    # own L4.handle, now retrying against the same candidate set instead
    # of a shrinking one).
    virtualisation.memorySize = 3072;
    virtualisation.cores = 2;
  };
in
{
  name = "expanse-db-postgres-vertical-slice";

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
    ${builtins.readFile ./python/db_postgres_vertical_slice.py}
  '';
}
