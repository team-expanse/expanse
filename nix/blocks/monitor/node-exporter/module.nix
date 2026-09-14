# Block runtime module for monitor/node-exporter (PHASE04.md §3.3/§6).
#
# Deployed with the daemonset strategy: one placement per eligible node.
# Minimal config — the exporter self-configures from /proc and /sys.
#
# Contract: import and call with:
#   {
#     pkgs             # nixpkgs
#     name             # instance name (systemd unit name)
#     port             # metrics port, from spec.config.port
#     webTelemetryPath # metrics URL path
#     user             # runtime user (default "expanse-block")
#   }
{ pkgs, name, port, webTelemetryPath, user ? "expanse-block" }:
{
  systemd.services."expanse-block-${name}" = {
    description = "expanse block ${name} (monitor/node-exporter)";
    wantedBy = [ "multi-user.target" ];
    after = [ "network.target" ];
    serviceConfig = {
      ExecStart =
        "${pkgs.prometheus-node-exporter}/bin/node_exporter "
        + "--web.listen-address=:${toString port} "
        + "--web.telemetry-path=${webTelemetryPath}";
      User = user;
      Restart = "always";
      RestartSec = "1s";
      NoNewPrivileges = true;
      ProtectHome = true;
      PrivateTmp = true;
    };
  };
}
