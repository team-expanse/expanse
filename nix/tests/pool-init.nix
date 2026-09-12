# Shared test helper: create the rpool ZFS pool + datasets + blank
# snapshot on a fresh disk on first boot, inside systemd stage 1.
#
# The VM test framework mounts the VM's own root disk as / (it overrides
# the whole fileSystems attrset; extra mounts must be declared via
# virtualisation.fileSystems). rpool/root is therefore mounted at
# /rpool-root and the tests assert the impermanence rollback wipes it on
# every boot while /persist survives. All impermanence mechanisms
# (rollback service, persist-init, bind mounts) run exactly as in
# production.
{ lib, pkgs, ... }:
let
  zfsMount = dataset: {
    device = dataset;
    fsType = "zfs";
    neededForBoot = true;
  };
in
{
  # zfs looks for disks under devNodes; whole-disk /dev/vdb pools.
  boot.zfs.devNodes = "/dev";

  # Make zfs available in systemd stage 1 (in production this happens
  # automatically because the root filesystem is on zfs; in the test VM
  # the root is the framework's disk, so we force it in).
  boot.initrd.availableKernelModules = [ "zfs" ];
  boot.initrd.kernelModules = [ "zfs" ];

  # Dataset + bind mounts, re-declared for the VM framework. Mirrors
  # storage.nix (minus "/", which the framework provides) and the bind
  # mounts from impermanence.nix.
  virtualisation.fileSystems = {
    "/persist" = zfsMount "rpool/persist";
    "/var/log" = zfsMount "rpool/var-log";
    "/rpool-root" = zfsMount "rpool/root";
    "/etc/machine-id" = {
      device = "/persist/etc/machine-id";
      fsType = "none";
      options = [ "bind" ];
      neededForBoot = true;
    };
    "/var/lib/nixos" = {
      device = "/persist/var/lib/nixos";
      fsType = "none";
      options = [ "bind" ];
      neededForBoot = true;
    };
    "/var/lib/systemd" = {
      device = "/persist/var/lib/systemd";
      fsType = "none";
      options = [ "bind" ];
      neededForBoot = true;
    };
    "/root/.ssh" = {
      device = "/persist/root/.ssh";
      fsType = "none";
      options = [ "bind" ];
      neededForBoot = true;
    };
  };

  boot.initrd.systemd.services.expanse-test-pool-init = {
    description = "Create test rpool on first boot";
    wantedBy = [ "initrd.target" ];
    before = [
      "zfs-import-rpool.service"
      "expanse-impermanence-rollback.service"
    ];
    after = [ "systemd-modules-load.service" ];
    unitConfig.DefaultDependencies = "no";
    serviceConfig.Type = "oneshot";
    script = ''
      # zfs checks the creator's hostid against /etc/hostid (binary,
      # little-endian). It does not persist in the initrd, so write it on
      # every boot. Must match expanse.hostId = "01234567".
      printf '\x67\x45\x23\x01' > /etc/hostid
      if ! zpool list -H rpool >/dev/null 2>&1; then
        # Pool not online. Try importing it from /dev (it exists on disk
        # from a previous boot); the zfs-import-rpool service will see it
        # online. If import fails, the pool truly does not exist yet.
        if ! zpool import -d /dev rpool >/dev/null 2>&1; then
          echo "expanse-test: creating rpool on /dev/vdb"
          zpool create -f -o ashift=12 -o autotrim=on \
            -O compression=zstd -O xattr=sa -O acltype=posixacl -O relatime=on \
            -O mountpoint=legacy rpool /dev/vdb
          zfs create rpool/root
          zfs create -o atime=off rpool/nix
          zfs create rpool/persist
          zfs create rpool/var-log
          zfs create -o mountpoint=none rpool/volumes
          zfs snapshot rpool/root@blank
        fi
      fi
    '';
  };
}
