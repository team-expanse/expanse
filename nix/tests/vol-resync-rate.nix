# Probe: full-resync time vs a busy writer for several DRBD c-min-rate values (A51).
# Not a gate; the scenario is python/vol_resync_rate_main.py and its RATE lines are the result.
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "vol-resync-rate-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    python3 -c 'import sys; compile(open(sys.argv[1]).read(), "testscript", "exec")' ${./python/vol_resync_rate_main.py}
    touch $out
  '';
  recorder = pkgs.runCommand "vol-resync-rate-rec" { } ''
    { echo "#!${pkgs.python3}/bin/python3"; cat ${./python/vol_durability_rec.py}; } > $out
    chmod +x $out
  '';
  node = { config, ... }: {
    boot.extraModulePackages = [ config.boot.kernelPackages.drbd ];
    services.drbd.enable = true;
    services.drbd.config = ''
      global { usage-count no; }
      include "/etc/drbd.d/*.res";
    '';
    systemd.services.drbd.wantedBy = lib.mkForce [ ];
    networking.firewall.enable = false;
    # 256 MiB matches the grow test's volume; the writer's 60000 records (234 MiB) fit.
    virtualisation.emptyDiskImages = [ 256 ];
    virtualisation.memorySize = 1024;
  };
in
{
  name = "expanse-vol-resync-rate";

  nodes = {
    n1 = node;
    n2 = node;
  };

  testScript = ''
    # ${lint}
    RECORDER = "${recorder}"
    REC = "/root/rate-rec"
    ${builtins.readFile ./python/vol_resync_rate_main.py}
  '';
}
