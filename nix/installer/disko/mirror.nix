# Multi-disk Expanse layout (A1, D8): btrfs RAID1 system partition
# mirrored across the first two disks, and the remainder of every disk
# -- including the mirrored pair's own remainder -- as plain LVM PVs in
# VG "expanse". No local RAID for data: cluster replication (DRBD) covers
# redundancy there, so extra disks are JBOD (see ARCHITECTURE.md §3.4).
# This replaces the old raidz1.nix; raidz1 was a ZFS concept with no
# in-tree equivalent worth reproducing.
#
# "system-b" is the side disko actually runs mkfs.btrfs on: disko
# partitions and formats disks in attribute-name order, so "system-a"'s
# raw partition already exists by the time "system-b" mkfs's the raid1
# pair into it. internal/install/plan.go (systemDevice) reads
# /dev/disk/by-partlabel/disk-system-b-root for the blank-snapshot and
# verify stages; keep the attribute names in sync with it if they change.
{ disks ? [ "/dev/sda" "/dev/sdb" ], ... }@args:
let
  systemSize = "8G";
  nDisks = builtins.length disks;
  subvolumes = {
    "@root" = { mountpoint = "/"; mountOptions = [ "compress=zstd" "noatime" ]; };
    "@nix" = { mountpoint = "/nix"; mountOptions = [ "compress=zstd" "noatime" ]; };
    "@persist" = { mountpoint = "/persist"; mountOptions = [ "compress=zstd" ]; };
    "@log" = { mountpoint = "/var/log"; mountOptions = [ "compress=zstd" ]; };
  };
  dataPartition = {
    size = "100%";
    content = {
      type = "lvm_pv";
      vg = "expanse";
    };
  };

  systemDisks = {
    system-a = {
      type = "disk";
      device = builtins.elemAt disks 0;
      content = {
        type = "gpt";
        partitions = {
          esp = {
            size = "1G";
            type = "EF00";
            content = {
              type = "filesystem";
              format = "vfat";
              mountpoint = "/boot";
              mountOptions = [ "umask=0077" ];
            };
          };
          # No content: left raw, contributing space to system-b's
          # mkfs.btrfs -d raid1 below.
          root = {
            size = systemSize;
          };
          data = dataPartition;
        };
      };
    };
    system-b = {
      type = "disk";
      device = builtins.elemAt disks 1;
      content = {
        type = "gpt";
        partitions = {
          root = {
            size = systemSize;
            content = {
              type = "btrfs";
              extraArgs = [ "-f" "-d" "raid1" "/dev/disk/by-partlabel/disk-system-a-root" ];
              inherit subvolumes;
            };
          };
          data = dataPartition;
        };
      };
    };
  };

  # Any disk beyond the mirrored pair is pure JBOD data, one PV each.
  extraDataDisks = builtins.listToAttrs (
    map
      (i: {
        name = "data-${toString i}";
        value = {
          type = "disk";
          device = builtins.elemAt disks i;
          content = {
            type = "gpt";
            partitions.data = dataPartition;
          };
        };
      })
      (if nDisks > 2 then builtins.genList (i: i + 2) (nDisks - 2) else [ ])
  );
in
{
  disko.devices = {
    disk = systemDisks // extraDataDisks;
    lvm_vg.expanse = {
      type = "lvm_vg";
      lvs = { };
    };
  };
}
