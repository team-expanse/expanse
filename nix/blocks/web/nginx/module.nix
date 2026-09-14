# Block runtime module for web/nginx (PHASE04.md §3.3/§6).
#
# Contract: import and call with:
#   {
#     pkgs       # nixpkgs
#     name       # instance name (systemd unit name)
#     port       # listen port, from spec.config.port
#     serverName # vhost ServerName
#     root       # document root
#     tls        # null or { cert; key; }
#     user       # runtime user (default "expanse-block")
#   }
{ pkgs, name, port, serverName, root, tls ? null, user ? "expanse-block" }:
let
  nginxConf = pkgs.writeText "expanse-block-${name}.conf" ''
    server {
      listen ${toString port};
      server_name ${serverName};
      root ${root};
      index index.html;
      location / {
        try_files $uri $uri/ =404;
      }
    }
  '';
  tlsConf = pkgs.writeText "expanse-block-${name}-tls.conf" ''
    server {
      listen ${toString port} ssl;
      server_name ${serverName};
      root ${root};
      index index.html;
      ssl_certificate     ${tls.cert};
      ssl_certificate_key ${tls.key};
      location / {
        try_files $uri $uri/ =404;
      }
    }
  '';
  conf = if tls != null then tlsConf else nginxConf;
in
{
  systemd.services."expanse-block-${name}" = {
    description = "expanse block ${name} (web/nginx)";
    wantedBy = [ "multi-user.target" ];
    after = [ "network.target" ];
    serviceConfig = {
      ExecStart = "${pkgs.nginx}/bin/nginx -c ${conf} -g 'daemon off; pid /tmp/expanse-block-${name}.pid; error_log /dev/stderr;'";
      User = user;
      Restart = "always";
      RestartSec = "1s";
      NoNewPrivileges = true;
      ProtectHome = true;
      PrivateTmp = true;
    };
  };
}
