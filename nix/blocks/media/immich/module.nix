# Block runtime module for media/immich.
#
# One Immich per instance, active/passive over the SINGLETON strategy like
# security/vaultwarden. expanse-block-run (immich.go) runs a private postgres
# with vectorchord, redis, the optional machine-learning service and the
# server, all with their state on the volume.
#
# Contract: import and call with:
#   {
#     pkgs        # nixpkgs
#     name        # instance name (systemd unit name)
#     user        # runtime user (default "expanse-block")
#   }
{ pkgs, name, user ? "expanse-block" }:
let
  unit = "expanse-block-${name}";
  postgres = pkgs.postgresql_18.withPackages (ps: [ ps.pgvector ps.vectorchord ]);
in
{
  systemd.services."${unit}" = {
    description = "expanse block ${name} (media/immich)";
    wantedBy = [ "multi-user.target" ];
    after = [ "network.target" ];
    path = [ postgres pkgs.redis pkgs.immich pkgs.immich.machine-learning ];
    serviceConfig = {
      ExecStart = "${pkgs.expanse}/bin/expanse-block-run ${unit}";
      User = user;
      Restart = "always";
      RestartSec = "1s";
      NoNewPrivileges = true;
      ProtectHome = true;
      PrivateTmp = true;
    };
  };
}
