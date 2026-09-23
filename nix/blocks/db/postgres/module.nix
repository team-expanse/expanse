# Block runtime module for db/postgres (PHASE-05-TASKS.md Stream A).
#
# Deployed as a plain active-active block (D3: replicas: N, no
# strategy.kind, replication: 1 per-replica volume) -- postgres's own
# streaming replication is the redundancy mechanism, not DRBD. Which
# replica is primary is decided by the lease-gated controller (D1, an
# agent-side reconciler, not this module's job) and handed to this unit
# through one file:
#
#   ${mountPath}/.expanse-postgres/role
#
# containing either "primary" or "replica <host> <port>". Only consulted
# on a replica's very first start (empty PGDATA) -- once initialized,
# postgres remembers its own role via standby.signal's presence, and
# promotion after that happens live via pg_promote() (D4), not a restart.
#
# Contract: import and call with:
#   {
#     pkgs                 # nixpkgs
#     name                 # instance name (systemd unit name)
#     port                 # postgres listen port, from spec.config.port
#     database             # initial database name, created at bootstrap
#     sharedBuffers        # shared_buffers, e.g. "256MB"
#     maxWalSenders        # max_wal_senders
#     maxReplicationSlots  # max_replication_slots
#     replicationPassword  # password for the internal 'replicator' role
#     superuserPassword    # password for the postgres superuser over TCP
#                          # (local Unix-socket admin access is trust)
#     mountPath            # data volume mount point (spec.storage[0].mountPath)
#     user                 # runtime user (default "expanse-block")
#   }
{ pkgs, name, port, database, sharedBuffers, maxWalSenders, maxReplicationSlots
, replicationPassword, superuserPassword, mountPath, user ? "expanse-block" }:
let
  pg = pkgs.postgresql_18; # R4: the default pkgs.postgresql on this pin is 17.11
  pgdata = "${mountPath}/pgdata";
  stateDir = "${mountPath}/.expanse-postgres";
  roleFile = "${stateDir}/role";
  sockDir = "${stateDir}/sock";
  unit = "expanse-block-${name}";

  # Same convention as db/redis's `password` field: the value ends up in
  # a world-readable /nix/store path either way on stock NixOS, so a
  # plain writeText is no worse -- but keeping it OUT of the generated
  # shell script's own source (see bootstrap below) avoids a real
  # injection bug a config-file interpolation like redis's doesn't have.
  superPwFile = pkgs.writeText "expanse-block-${name}-pg-superpw" superuserPassword;
  replPwFile = pkgs.writeText "expanse-block-${name}-pg-replpw" replicationPassword;

  # 10.42.0.0/16: the WireGuard overlay CIDR (internal/network/addrplan),
  # the only network this block's replication/admin traffic ever crosses.
  hba = pkgs.writeText "expanse-block-${name}-pg_hba.conf" ''
    local   all             all                                     trust
    host    replication     replicator      10.42.0.0/16            scram-sha-256
    host    all             all             10.42.0.0/16            scram-sha-256
  '';

  conf = pkgs.writeText "expanse-block-${name}-postgresql.conf" ''
    listen_addresses = '0.0.0.0'
    port = ${toString port}
    unix_socket_directories = '${sockDir}'
    shared_buffers = ${sharedBuffers}
    wal_level = replica
    hot_standby = on
    max_wal_senders = ${toString maxWalSenders}
    max_replication_slots = ${toString maxReplicationSlots}
    # Required for pg_rewind (X4) -- a postmaster-context GUC, cheaper to
    # set once at bootstrap than to rediscover the need for it in Stream C.
    wal_log_hints = on
    hba_file = '${hba}'
  '';

  bootstrap = pkgs.writeShellApplication {
    name = "expanse-block-${name}-pg-bootstrap";
    runtimeInputs = [ pg ];
    text = ''
      install -d -m0700 "${pgdata}"
      install -d -m0770 "${sockDir}"
      install -d -m0700 "${stateDir}"

      if [ -s "${pgdata}/PG_VERSION" ]; then
        # Already initialized: postgres remembers its own role via
        # standby.signal's presence. Nothing left to bootstrap.
        exit 0
      fi

      # First-ever start: wait for the controller's role decision rather
      # than guessing -- it alone knows the lease state and peer set.
      deadline=$((SECONDS + 120))
      while [ ! -s "${roleFile}" ]; do
        if [ "$SECONDS" -ge "$deadline" ]; then
          echo "pg-bootstrap: no role at ${roleFile} after 120s" >&2
          exit 1
        fi
        sleep 1
      done
      read -r kind host peerport < "${roleFile}"

      SUPERUSER_PASSWORD=$(cat "${superPwFile}")
      REPLICATION_PASSWORD=$(cat "${replPwFile}")

      case "$kind" in
        primary)
          pwfile=$(mktemp)
          printf '%s' "$SUPERUSER_PASSWORD" > "$pwfile"
          initdb -D "${pgdata}" --username=postgres --pwfile="$pwfile"
          rm -f "$pwfile"
          cp "${conf}" "${pgdata}/postgresql.conf"
          pg_ctl -D "${pgdata}" -w start
          psql -h "${sockDir}" -p ${toString port} -U postgres -v ON_ERROR_STOP=1 \
            -v pass="$REPLICATION_PASSWORD" -v dbname="${database}" <<SQL
CREATE ROLE replicator WITH REPLICATION LOGIN PASSWORD :'pass';
CREATE DATABASE :"dbname";
SQL
          pg_ctl -D "${pgdata}" -w stop
          ;;
        replica)
          PGPASSWORD="$REPLICATION_PASSWORD" pg_basebackup \
            -h "$host" -p "$peerport" -U replicator \
            -D "${pgdata}" -Fp -Xs -R -C -S "expanse_${name}"
          cp "${conf}" "${pgdata}/postgresql.conf"
          ;;
        *)
          echo "pg-bootstrap: unrecognised role kind '$kind' in ${roleFile}" >&2
          exit 1
          ;;
      esac
      chmod 0700 "${pgdata}"
    '';
  };
in
{
  systemd.services."${unit}" = {
    description = "expanse block ${name} (db/postgres)";
    wantedBy = [ "multi-user.target" ];
    after = [ "network.target" ];
    serviceConfig = {
      ExecStartPre = "${bootstrap}/bin/expanse-block-${name}-pg-bootstrap";
      ExecStart = "${pg}/bin/postgres -D ${pgdata}";
      User = user;
      Restart = "always";
      RestartSec = "1s";
      NoNewPrivileges = true;
      ProtectHome = true;
      PrivateTmp = true;
    };
  };
}
