# Multi-disk Expanse layout (A1, D8): the first two disks each carry an ESP
# and a system partition, mirrored as md RAID1 arrays "esp" (/boot) and
# "system" (btrfs), so either disk alone boots. The remainder of every disk
# is a plain LVM PV in VG "expanse": DRBD replicates data across nodes, so
# extra disks are JBOD (see ARCHITECTURE.md §3.4).
#
# The ESP array uses metadata 1.0 (superblock at the end), so firmware reads
# each half as a plain FAT filesystem; nix/modules/md-boot.nix lets bootctl
# install onto it. internal/install/plan.go (systemDevice) snapshots
# /dev/md/system; keep the array names in sync with it.
{ disks ? [ "/dev/sda" "/dev/sdb" ], ... }@args:
let
  systemSize = "8G";
  nDisks = builtins.length disks;
  dataPartition = {
    size = "100%";
    content = {
      type = "lvm_pv";
      vg = "expanse";
    };
  };

  systemDisk = device: {
    type = "disk";
    inherit device;
    content = {
      type = "gpt";
      partitions = {
        esp = {
          size = "1G";
          type = "EF00";
          content = { type = "mdraid"; name = "esp"; };
        };
        system = {
          size = systemSize;
          type = "FD00";
          content = { type = "mdraid"; name = "system"; };
        };
        data = dataPartition;
      };
    };
  };
  systemDisks = {
    system-a = systemDisk (builtins.elemAt disks 0);
    system-b = systemDisk (builtins.elemAt disks 1);
  };

  # A newly created array still exposes the data of any array its partitions held before, and
  # disko formats only blank devices: wipe a new array so a reinstall gets fresh filesystems.
  freshArray = name: {
    type = "mdadm";
    level = 1;
    preCreateHook = ''
      test -e /dev/md/${name} || touch "$disko_devices_dir/new-md-${name}"
    '';
  };
  wipeIfNew = name: ''
    if [ -e "$disko_devices_dir/new-md-${name}" ]; then wipefs --all /dev/md/${name}; fi
  '';

  mdArrays = {
    esp = freshArray "esp" // {
      metadata = "1.0";
      content = {
        preCreateHook = wipeIfNew "esp";
        type = "filesystem";
        format = "vfat";
        mountpoint = "/boot";
        mountOptions = [ "umask=0077" ];
      };
    };
    system = freshArray "system" // {
      content = {
        preCreateHook = wipeIfNew "system";
        type = "btrfs";
        extraArgs = [ "-f" ];
        subvolumes = {
          "@root" = { mountpoint = "/"; mountOptions = [ "compress=zstd" "noatime" ]; };
          "@nix" = { mountpoint = "/nix"; mountOptions = [ "compress=zstd" "noatime" ]; };
          "@persist" = { mountpoint = "/persist"; mountOptions = [ "compress=zstd" ]; };
          "@log" = { mountpoint = "/var/log"; mountOptions = [ "compress=zstd" ]; };
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
    mdadm = mdArrays;
    lvm_vg.expanse = {
      type = "lvm_vg";
      lvs = { };
    };
  };
}
