# Single-disk Expanse layout: ESP + ZFS rpool.
# The disk list is passed as an argument by the installer:
#   disko --arg disks '["/dev/sda"]' this-file.nix
{ disks ? [ "/dev/sda" ], poolMode ? "", ... }@args:
let
  # All layouts share the same dataset structure; only the pool vdevs
  # differ, so we build the pool config in a helper.
  mkDatasets = {
    root = {
      type = "zfs_fs";
      mountpoint = "/";
      options = {
        mountpoint = "legacy";
        compression = "zstd";
      };
    };
    nix = {
      type = "zfs_fs";
      mountpoint = "/nix";
      options = {
        mountpoint = "legacy";
        compression = "zstd";
        atime = "off";
      };
    };
    persist = {
      type = "zfs_fs";
      mountpoint = "/persist";
      options = {
        mountpoint = "legacy";
        compression = "zstd";
        xattr = "sa";
        acltype = "posixacl";
      };
    };
    var-log = {
      type = "zfs_fs";
      mountpoint = "/var/log";
      options = {
        mountpoint = "legacy";
        compression = "zstd";
      };
    };
    volumes = {
      type = "zfs_fs";
      options = {
        mountpoint = "none";
      };
    };
  };
in
{
  disko.devices = {
    disk = builtins.listToAttrs (map (dev: {
      name = builtins.replaceStrings [ "/" ] [ "_" ] dev;
      value = {
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
            zfs = {
              size = "100%";
              content = {
                type = "zfs";
                pool = "rpool";
              };
            };
          };
        };
      };
    }) disks);

    zpool.rpool = {
      type = "zpool";
      mode = poolMode;
      # Pool properties (-o) vs inherited dataset defaults (-O):
      # ashift/autotrim are pool-level; compression/xattr/acltype/relatime
      # are dataset properties inherited by every dataset. (zfs 2.4
      # rejects dataset properties passed as -o at pool creation.)
      options = {
        ashift = "12";
        autotrim = "on";
      };
      rootFsOptions = {
        compression = "zstd";
        xattr = "sa";
        acltype = "posixacl";
        relatime = "on";
        mountpoint = "legacy";
      };
      datasets = mkDatasets;
    };
  };
}
