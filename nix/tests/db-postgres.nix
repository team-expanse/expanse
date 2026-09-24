# PHASE-05-TASKS.md Stream A: deploy a db/postgres block with streaming
# replication across 3 active-active replicas; an external client
# connects to the block's stable VIP endpoint and runs read/write
# queries against whichever replica the lease-gated election controller
# (D1) currently names primary, routed there by the LB's PrimaryOnly
# mode (D2) rather than round-robin (X1). Combines storage-test.nix
# (DRBD/LVM, Phase 1, D3's per-replica volumes) with blocksCatalog
# (Phase 04) and an external VIP pool (Phase 05), the same substrate
# share-smb.nix (Phase 3 X1) already proved for a SINGLETON block — this
# is the first VM test to exercise the plain N-replica shape with bound
# storage and application-level (not DRBD-promotion) primary election.
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "db-postgres-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    for f in ${./cluster-common.py} ${./block-common.py} ${./client-common.py} ${./python/vol_cluster.py} ${./python/db_postgres.py}; do
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
    expanse.agent.period = "5s";
    expanse.agent.controllerPeriod = "5s";
    expanse.agent.blocksCatalog = ../blocks;
    expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
    environment.etc."expanse/blocks-flake".source = ../blocks-flake;
    expanse.agent.externalVIPPool = "${builtins.elemAt vipPool 0}-${builtins.elemAt vipPool 1}";
    expanse.agent.externalInterface = "eth1";
    # postgresql_18 (R4, ARCHITECTURE.md A28's evidence base): the
    # default pkgs.postgresql on this nixpkgs pin is 17.11. Provides
    # postgres/initdb/pg_ctl/pg_basebackup/psql on PATH — the same
    # "upstream binary shipped in the deployment image, exec'd from
    # PATH" convention share-smb.nix's samba package already uses.
    environment.systemPackages = [ pkgs.curl pkgs.jq pkgs.postgresql_18 ];
    # PORT (the VIP-exposed, client-facing port) and PG_PORT (postgres's
    # own internal listen port, used directly node-to-node for
    # streaming replication/pg_basebackup, bypassing the VIP/LB
    # entirely) both need to be open on every node, since any of the 3
    # could hold the VIP or be the elected primary. Missed on the first
    # pass (share-smb.nix's own analogous 445 was the precedent to
    # follow) -- pg_basebackup timed out reaching the primary directly
    # until this was added.
    networking.firewall.allowedTCPPorts = [ 5432 55432 ];
    virtualisation.memorySize = 2048;
  };
in
{
  name = "expanse-db-postgres";

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
    ${builtins.readFile ./python/db_postgres.py}
  '';
}
