# Phase 1 B2: DRBD's own parser must accept every config the generator emits.
{ self }:
{ pkgs, lib, ... }:
let
  golden = ../../test/fixtures/drbd/config;
  lint = pkgs.runCommand "vol-drbd-config-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    python3 -c 'import sys; compile(open(sys.argv[1]).read(), "testscript", "exec")' ${./python/vol_drbd_config_main.py}
    touch $out
  '';
in
{
  name = "expanse-vol-drbd-config";

  nodes.n1 = { config, ... }: {
    boot.extraModulePackages = [ config.boot.kernelPackages.drbd ];
    services.drbd.enable = true;
    services.drbd.config = ''
      global { usage-count no; }
    '';
    systemd.services.drbd.wantedBy = lib.mkForce [ ];
    virtualisation.memorySize = 512;
  };

  testScript = ''
    # ${lint}
    ${builtins.replaceStrings [ "@golden@" ] [ "${golden}" ] (builtins.readFile ./python/vol_drbd_config_main.py)}
  '';
}
