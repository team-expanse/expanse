# PHASE-05-TASKS.md Stream B (X2, the phase's decider): pgbench sustains
# continuous load against a db/postgres block while the node running the
# current primary is hard-killed. A surviving replica promotes itself
# automatically (pgha's lease-loss path, D1/D4), the external client's
# connection to the same stable VIP endpoint resumes without a manual
# reconnect, and every transaction the client believed committed before
# the kill is actually present on the promoted primary -- no acknowledged
# write lost (D5's split-brain guard plus synchronous_standby_names make
# this true, or this test is what finds out otherwise). Combines
# db-postgres.nix's substrate (Stream A) with share-smb-failover.nix's
# kill/reconverge pattern (Phase 03 Stream B2).
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "db-postgres-failover-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    for f in ${./cluster-common.py} ${./client-common.py} ${./block-common.py} ${./python/vol_cluster.py} ${./python/db_postgres_failover.py}; do
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
    # Tight reconcile period: pgha's own promotion-on-loss retry (every
    # pass) and the LB pool's PrimaryOnly routing both need to converge
    # within the test's failover budget.
    expanse.agent.period = "5s";
    expanse.agent.controllerPeriod = "5s";
    expanse.agent.blocksCatalog = ../blocks;
    expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
    environment.etc."expanse/blocks-flake".source = ../blocks-flake;
    expanse.agent.externalVIPPool = "${builtins.elemAt vipPool 0}-${builtins.elemAt vipPool 1}";
    expanse.agent.externalInterface = "eth1";
    environment.systemPackages = [ pkgs.curl pkgs.jq pkgs.postgresql_18 ];
    networking.firewall.allowedTCPPorts = [ 5432 55432 ];
    virtualisation.memorySize = 2048;
  };
in
{
  name = "expanse-db-postgres-failover";

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
    ${builtins.readFile ./python/db_postgres_failover.py}
  '';
}
