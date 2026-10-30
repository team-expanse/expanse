# What each block type's workload execs (cmd/expanse-block-run), one environment per type.
# The node bakes in vm/instance's runtime; the agent realises the rest when a type is first placed.
{ pkgs }:
let
  # Headless: runVM uses -display none and a serial console, so no GUI, audio or spice (1.5 GiB -> 0.5).
  qemu = pkgs.qemu.override {
    hostCpuOnly = true;
    nixosTestRunner = true;
    guestAgentSupport = false;
    libiscsiSupport = false;
    tpmSupport = false;
    brlttySupport = false;
    capstoneSupport = false;
    enableDocs = false;
  };
  # Garage 1.3.1 sets LMDB's MdbNoMetaSync even with metadata_fsync, so a crash could undo the last
  # acknowledged write; skip it when fsync is on (upstream patch pending).
  garage = pkgs.garage.overrideAttrs (old: {
    patches = (old.patches or [ ]) ++ [ ./patches/garage-lmdb-meta-sync.patch ];
  });
  # One postgres serves db/postgres and media/immich, which needs these extensions.
  postgres = pkgs.postgresql_18.withPackages (ps: [ ps.pgvector ps.vectorchord ]);
  packages = with pkgs; {
    "ai/ollama" = [ ollama ];
    "db/mariadb" = [ mariadb ];
    "db/postgres" = [ postgres ];
    "db/redis" = [ redis ];
    "dev/forgejo" = [ forgejo git ];
    "iscsi/target" = [ targetcli-fb ];
    "media/immich" = [ immich immich-machine-learning postgres redis ];
    "media/jellyfin" = [ jellyfin ];
    "monitor/grafana" = [ grafana (callPackage ./grafana-plugins.nix { }) ];
    "monitor/node-exporter" = [ prometheus-node-exporter ];
    "monitor/prometheus" = [ prometheus ];
    "monitor/uptime-kuma" = [ uptime-kuma ];
    "net/haproxy" = [ haproxy ];
    "security/vaultwarden" = [ vaultwarden vaultwarden.webvault ];
    "share/nfs" = [ nfs-ganesha ];
    "share/smb" = [ samba ];
    "storage/s3" = [ garage ];
    "util/restic-backup" = [ restic ];
    # xorriso builds the cloud-init seed ISO.
    "vm/instance" = [ qemu xorriso ];
    "web/caddy" = [ caddy ];
    "web/nginx" = [ nginx ];
  };
in
builtins.mapAttrs
  (type: paths: pkgs.buildEnv {
    name = "expanse-runtime-${builtins.replaceStrings [ "/" ] [ "-" ] type}";
    inherit paths;
  })
  packages
