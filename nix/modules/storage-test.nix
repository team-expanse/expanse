# Storage test substrate for the Phase 1 vol-*.nix VM tests: a scratch LVM volume
# group with a thin pool on an added virtual disk, plus the DRBD kernel module and
# tooling, wired into the agent.
#
# VG creation is idempotent and never wipes: a crash-restored VM keeps its disk, so
# an existing VG is activated and left alone (recreating it would destroy every
# volume on it).
{ config, pkgs, lib, ... }:
let
  cfg = config.expanse.storage-test;
in
{
  options.expanse.storage-test = {
    enable = lib.mkEnableOption "scratch LVM volume group and DRBD for storage VM tests";

    vgName = lib.mkOption {
      type = lib.types.str;
      default = "vg0";
      description = "Name of the scratch volume group.";
    };

    poolPercent = lib.mkOption {
      type = lib.types.ints.between 10 100;
      default = 90;
      description = "Share of the volume group's free space the thin pool takes; the rest stays for thick LVs.";
    };

    poolName = lib.mkOption {
      type = lib.types.str;
      default = "pool";
      description = "Name of the thin pool created in the volume group.";
    };

    diskSizeMB = lib.mkOption {
      type = lib.types.int;
      default = 2048;
      description = "Size of the added virtual disk backing the volume group, in MiB.";
    };
  };

  config = lib.mkIf cfg.enable {
    # /dev/vdb backing the volume group (the framework's own disk is vda).
    virtualisation.emptyDiskImages = [ cfg.diskSizeMB ];

    # DRBD 9.2.16 does not build against the latest kernel series; base.nix now pins
    # every node to the LTS series for production (A2). mkForce here is belt-and-braces
    # so this module keeps working stand-alone if that ever changes.
    boot.kernelPackages = lib.mkForce pkgs.linuxPackages;
    boot.extraModulePackages = [ config.boot.kernelPackages.drbd ];
    boot.kernelModules = [ "drbd" ];
    services.drbd.enable = true;
    services.drbd.config = ''
      global { usage-count no; }
      include "/etc/drbd.d/*.res";
    '';
    # Resources are brought up by the agent, not by the drbd unit.
    systemd.services.drbd.wantedBy = lib.mkForce [ ];

    services.lvm.enable = true;
    services.lvm.boot.thin.enable = true;
    environment.systemPackages = [ pkgs.lvm2 pkgs.thin-provisioning-tools pkgs.e2fsprogs ];

    # DRBD replication runs over the mesh; the agent's ruleset admits the same range in production (vol-firewall).
    networking.firewall.interfaces.exp0.allowedTCPPortRanges = [{ from = 9500; to = 10499; }];

    expanse.agent.storageVG = cfg.vgName;
    expanse.agent.storagePool = cfg.poolName;

    systemd.services.expanse-scratch-vg = {
      description = "Create the scratch LVM volume group for storage tests";
      wantedBy = [ "multi-user.target" ];
      before = [ "expansed.service" ];
      after = [ "systemd-modules-load.service" "systemd-udev-settle.service" ];
      path = [ pkgs.lvm2 pkgs.thin-provisioning-tools ];
      unitConfig.DefaultDependencies = "no";
      serviceConfig.Type = "oneshot";
      serviceConfig.RemainAfterExit = true;
      script = ''
        vgchange -ay ${cfg.vgName} >/dev/null 2>&1 || true
        if vgs ${cfg.vgName} >/dev/null 2>&1; then
          echo "storage-test: volume group ${cfg.vgName} already exists"
          exit 0
        fi
        vgcreate ${cfg.vgName} /dev/vdb
        lvcreate --yes --type thin-pool -l ${toString cfg.poolPercent}%FREE -n ${cfg.poolName} ${cfg.vgName}
      '';
    };
  };
}
