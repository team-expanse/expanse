# Spike (Phase 06 T23 follow-up): would DRBD 9 on the same zvols meet the
# §5 perf budgets and fail over cleanly? Same fio profiles, same raw
# baseline, same budgets.yaml as vol-perf, but the replication layer is
# the in-kernel DRBD instead of exvol. Not part of the product; it exists
# to answer a build-vs-adopt question with measurements.
{ self }:
{ pkgs, lib, ... }:
let
  peers = { n1 = "192.168.1.1"; n2 = "192.168.1.2"; n3 = "192.168.1.3"; };
  nodeId = { n1 = 0; n2 = 1; n3 = 2; };
  onBlocks = lib.concatStrings (lib.mapAttrsToList (n: ip: ''
    on ${n} { node-id ${toString nodeId.${n}}; address ${ip}:7789; }
  '') peers);
  drbdConf = ''
    global { usage-count no; }
    resource r0 {
      device /dev/drbd0 minor 0;
      disk /dev/zvol/volumes/drbdperf;
      meta-disk internal;
      net { protocol C; ping-int 2; ping-timeout 10; timeout 30; }
      options { quorum majority; on-no-quorum io-error; }
      ${onBlocks}
      connection-mesh { hosts n1 n2 n3; }
    }
  '';
  nodeCommon = idx: { config, ... }: {
    imports = [ ../modules/storage-test.nix ];
    boot.supportedFilesystems = [ "zfs" ];
    boot.zfs.forceImportRoot = false;
    networking.hostId = "0000000${toString idx}";
    expanse.storage-test.enable = true;
    expanse.storage-test.poolSizeMB = 8192;
    virtualisation.memorySize = 2048;
    virtualisation.diskSize = 16 * 1024;

    # The in-tree drbd is 8.4 (two nodes); 3-way needs the 9.x module.
    boot.extraModulePackages = [ config.boot.kernelPackages.drbd ];
    services.drbd.enable = true;
    services.drbd.config = drbdConf;
    # Resources have no metadata until the test creates it.
    systemd.services.drbd.wantedBy = lib.mkForce [ ];
    networking.firewall.allowedTCPPorts = [ 7789 ];
    environment.systemPackages = with pkgs; [ zfs python3 fio iputils ];
  };
  budgetsJson = pkgs.runCommand "budgets.json" { nativeBuildInputs = [ pkgs.yq-go ]; } ''
    yq -o=json '.budgets' ${../../test/perf/budgets.yaml} > $out
  '';
  budgetsPy = "BUDGETS = json.loads(open('${budgetsJson}').read())\n";
  lint = pkgs.runCommand "vol-drbd-spike-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    cd ${./python}
    { cat vol_perf_lib.py; printf '%s\n' ${lib.escapeShellArg budgetsPy}; cat vol_perf_fio.py vol_drbd_spike_main.py; } \
      | python3 -c 'import sys; compile(sys.stdin.read(), "testscript", "exec")'
    touch $out
  '';
in
{
  name = "expanse-vol-drbd-spike";

  nodes = {
    n1 = nodeCommon 1;
    n2 = nodeCommon 2;
    n3 = nodeCommon 3;
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./python/vol_perf_lib.py}
    ${budgetsPy}
    ${builtins.readFile ./python/vol_perf_fio.py}
    ${builtins.readFile ./python/vol_drbd_spike_main.py}
  '';
}
