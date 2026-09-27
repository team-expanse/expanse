# Phase 2, A2: the web UI's session/auth layer -- argon2id-hashed admin
# credential (D3/D4), store-backed sessions (D2), login/logout. Builds
# on ui-scaffold.nix's TLS acceptance; this is the auth layer's own gate.
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "ui-auth-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    cd ${./python}
    cat ${./cluster-common.py} ui_auth.py \
      | python3 -c 'import sys; compile(sys.stdin.read(), "testscript", "exec")'
    touch $out
  '';
  nodeCommon = idx: {
    imports = [ self.nixosModules.expanse ];
    nixpkgs.overlays = [
      (final: prev: { expanse = self.packages.${prev.system}.expanse; })
    ];
    environment.systemPackages = [ pkgs.curl pkgs.openssl ];
    expanse.node.enable = true;
    expanse.agent.enable = true;
    expanse.hostId = "0000000${toString idx}";
    expanse.hostname = "n${toString idx}";
    virtualisation.memorySize = 1536;
  };
in
{
  name = "expanse-ui-auth";

  nodes = {
    n1 = { ... }: nodeCommon 1;
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./python/ui_auth.py}
  '';
}
