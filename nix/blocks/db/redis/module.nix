# Block runtime module for db/redis (PHASE04.md §3.3/§6).
#
# Deployed with the primary-replica strategy. The data directory lives on
# the block's storage volume (mountPath) so persistence survives restarts.
#
# Contract: import and call with:
#   {
#     pkgs            # nixpkgs
#     name            # instance name (systemd unit name)
#     port            # listen port, from spec.config.port
#     maxMemory       # maxmemory value (e.g. "256mb")
#     maxMemoryPolicy # maxmemory-policy
#     appendOnly      # AOF enabled?
#     replicaOf       # null or "host:port" replication source
#     password        # null or requirepass value
#     mountPath       # data volume mount point (spec.storage[0].mountPath)
#     user            # runtime user (default "expanse-block")
#   }
{ pkgs, name, port, maxMemory, maxMemoryPolicy, appendOnly, replicaOf ? null
, password ? null, mountPath, user ? "expanse-block" }:
let
  conf = pkgs.writeText "expanse-block-${name}.conf" ''
    port ${toString port}
    bind 0.0.0.0
    dir ${mountPath}
    dbfilename dump.rdb
    appendonly ${if appendOnly then "yes" else "no"}
    maxmemory ${maxMemory}
    maxmemory-policy ${maxMemoryPolicy}
    ${if replicaOf != null then "replicaof ${replicaOf}" else ""}
    ${if password != null then "requirepass ${password}" else ""}
  '';
in
{
  systemd.services."expanse-block-${name}" = {
    description = "expanse block ${name} (db/redis)";
    wantedBy = [ "multi-user.target" ];
    after = [ "network.target" ];
    serviceConfig = {
      ExecStart = "${pkgs.redis}/bin/redis-server ${conf}";
      User = user;
      Restart = "always";
      RestartSec = "1s";
      NoNewPrivileges = true;
      ProtectHome = true;
      PrivateTmp = true;
    };
  };
}
