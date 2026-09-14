# Block runtime module for web/static-site (PHASE04.md §3.3/§6).
#
# Exercises spec.storage: the site is served from the block's mounted
# volume (mountPath), not from the unit's own files — Phase 06 wires the
# real volume attach; the module already serves from mountPath so the
# V12/V13 storage rules are exercised end-to-end once it lands.
#
# Contract: import and call with:
#   {
#     pkgs      # nixpkgs
#     name      # instance name (systemd unit name)
#     port      # listen port, from spec.config.port
#     index     # HTML written to the volume on start
#     mountPath # volume mount point (spec.storage[0].mountPath)
#     user      # runtime user (default "expanse-block")
#   }
{ pkgs, name, port, index, mountPath, user ? "expanse-block" }:
let
  # systemd Environment values are not shell-parsed; strip characters
  # that would break the unit file.
  envIndex = builtins.replaceStrings
    [ "\n" "\r" "\"" "\\" ]
    [ " " " " "' " "-" ]
    index;
in
{
  systemd.services."expanse-block-${name}" = {
    description = "expanse block ${name} (web/static-site)";
    wantedBy = [ "multi-user.target" ];
    after = [ "network.target" ];
    serviceConfig = {
      # Write the site into the mounted volume, then serve it.
      ExecStartPre =
        "${pkgs.coreutils}/bin/install -d ${mountPath} && "
        + ''echo "$SITE_INDEX" > ${mountPath}/index.html'';
      ExecStart =
        "${pkgs.python3}/bin/python -m http.server ${toString port} "
        + "--directory ${mountPath}";
      Environment = [ "SITE_INDEX=${envIndex}" ];
      User = user;
      Restart = "always";
      RestartSec = "1s";
      NoNewPrivileges = true;
      ProtectHome = true;
      PrivateTmp = true;
    };
  };
}
