# Block runtime module for util/echo (PHASE04.md §3.3/§6).
#
# Contract: import this file and call it with an attribute set:
#   {
#     pkgs   # nixpkgs
#     name   # instance name (used for the systemd unit name)
#     port   # listen port, from spec.config.port
#     body   # echo response body, from spec.config.body
#     user   # runtime user (default "expanse-block")
#   }
# It returns an attribute set of NixOS config fragments to merge (the
# generated systemd unit). The echo server is a single stdlib-only Go
# binary compiled at build time — it starts in well under a second.
{ pkgs, name, port, body, user ? "expanse-block" }:
let
  echoServer = pkgs.runCommand "echo-server" { } ''
    mkdir -p $out/bin
    cp ${./echo.go} echo.go
    export GOCACHE=$TMPDIR/go-build
    export GOPATH=$TMPDIR/gopath
    export GOTOOLCHAIN=local
    export GOPROXY=off
    ${pkgs.go}/bin/go build -trimpath -o $out/bin/echo-server ./echo.go
  '';

  # systemd Environment values are not shell-parsed; strip characters that
  # would break the unit file (newlines, quotes, backslashes).
  envBody = builtins.replaceStrings
    [ "\n" "\r" "\"" "\\" ]
    [ " " " " "' " "-" ]
    body;
in
{
  systemd.services."expanse-block-${name}" = {
    description = "expanse block ${name} (util/echo)";
    wantedBy = [ "multi-user.target" ];
    after = [ "network.target" ];
    serviceConfig = {
      ExecStart = "${echoServer}/bin/echo-server";
      User = user;
      Environment = [
        "ECHO_PORT=${toString port}"
        "ECHO_BODY=${envBody}"
      ];
      Restart = "always";
      RestartSec = "1s";
      NoNewPrivileges = true;
      ProtectSystem = "strict";
      ProtectHome = true;
      PrivateTmp = true;
    };
  };
}
