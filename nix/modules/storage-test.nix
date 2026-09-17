# Storage test pool: provisions a scratch ZFS pool for Phase 06 vol-*.nix
# VM tests on cluster-test nodes that have no real data disks.
#
# Adds a second virtual disk and creates a sparse `volumes` pool on it at
# boot, before the agent starts. Every exvol volume in VM tests lives at
# volumes/volumes/<vol-id> (zvol paths mirror production's
# rpool/volumes/<vol-id> shape with a test pool name).
{ config, pkgs, lib, ... }:
let
  cfg = config.expanse.storage-test;
in
{
  options = {
    expanse.storage-test.enable = lib.mkEnableOption "scratch ZFS pool for storage VM tests";

    expanse.storage-test.poolName = lib.mkOption {
      type = lib.types.str;
      default = "volumes";
      description = "Name of the scratch pool created for exvol volumes in VM tests.";
    };

    expanse.storage-test.poolSizeMB = lib.mkOption {
      type = lib.types.int;
      default = 8192;
      description = "Size of the added virtual disk backing the scratch pool, in MiB.";
    };
  };

  config = lib.mkIf cfg.enable {
    # /dev/vdb backing the scratch pool (the framework's own disk is vda).
    virtualisation.emptyDiskImages = [ { size = cfg.poolSizeMB; } ];

    boot.zfs.devNodes = "/dev";

    # Zvol device nodes (/dev/<pool>/<ds>) are udev symlinks; without
    # the zfs rules installed the daemon would never see them.
    services.udev.packages = [ pkgs.zfs ];

    systemd.services.expanse-scratch-pool = {
      description = "Create scratch ZFS pool for exvol storage tests";
      wantedBy = [ "multi-user.target" ];
      before = [ "expansed.service" ];
      after = [ "systemd-modules-load.service" "systemd-udev-settle.service" ];
      path = with pkgs; [ zfs ];
      unitConfig.DefaultDependencies = "no";
      serviceConfig.Type = "oneshot";
      serviceConfig.RemainAfterExit = true;
      script = ''
        if zpool list ${cfg.poolName} >/dev/null 2>&1; then
          echo "expanse-storage-test: pool ${cfg.poolName} already exists"
          exit 0
        fi
        # A restored (crash-tested) VM's disk label survives the reboot;
        # cachefile=none just means it is not auto-imported. Import it —
        # `zpool create -f` here would silently WIPE the pool and
        # destroy every volume on it (which is how a resync test's
        # restored node came back with a fresh, empty zvol).
        if zpool import -f ${cfg.poolName} >/dev/null 2>&1; then
          echo "expanse-storage-test: imported existing pool ${cfg.poolName}"
          exit 0
        fi
        echo "expanse-storage-test: creating pool ${cfg.poolName}"
        zpool create -f -o cachefile=none ${cfg.poolName} /dev/vdb
        zfs create ${cfg.poolName}/volumes
      '';
    };
  };
}
