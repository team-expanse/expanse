# Single-disk Expanse layout (A1): ESP + a fixed-size btrfs system
# partition (subvolumes @root, @nix, @persist, @log) + the disk's
# remainder as an LVM PV in VG "expanse", which the agent thin-provisions
# DRBD-backed volumes from. See ARCHITECTURE.md §3.1.
#
# The "system" partition label below is read by internal/install/plan.go
# (systemDevice) to find /dev/disk/by-partlabel/disk-system-root for the
# blank-snapshot and verify stages; keep the disk/partition attribute
# names in sync with it if they change.
{ disks ? [ "/dev/sda" ], ... }@args:
let
  dev = builtins.elemAt disks 0;
  # Generous for root+nix+persist+log with room to grow; the remainder of
  # the disk -- most of it, in production -- goes to LVM.
  systemSize = "8G";
  subvolumes = {
    "@root" = { mountpoint = "/"; mountOptions = [ "compress=zstd" "noatime" ]; };
    "@nix" = { mountpoint = "/nix"; mountOptions = [ "compress=zstd" "noatime" ]; };
    "@persist" = { mountpoint = "/persist"; mountOptions = [ "compress=zstd" ]; };
    "@log" = { mountpoint = "/var/log"; mountOptions = [ "compress=zstd" ]; };
  };
in
{
  disko.devices = {
    disk.system = {
      type = "disk";
      device = dev;
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
          root = {
            size = systemSize;
            content = {
              type = "btrfs";
              extraArgs = [ "-f" ];
              inherit subvolumes;
            };
          };
          data = {
            size = "100%";
            content = {
              type = "lvm_pv";
              vg = "expanse";
            };
          };
        };
      };
    };
    lvm_vg.expanse = {
      type = "lvm_vg";
      lvs = { };
    };
  };
}
