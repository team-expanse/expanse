# ZFS dataset filesystem declarations for an installed Expanse node.
# Datasets are created by disko at install time with mountpoint=legacy,
# so they are mounted via the standard fileSystems mechanism, ordered
# correctly against the impermanence rollback.
{ config, pkgs, lib, ... }:
{
  config = lib.mkIf config.expanse.node.enable {
    fileSystems = {
      "/" = {
        device = "rpool/root";
        fsType = "zfs";
        neededForBoot = true;
      };
      "/nix" = {
        device = "rpool/nix";
        fsType = "zfs";
        neededForBoot = true;
      };
      "/persist" = {
        device = "rpool/persist";
        fsType = "zfs";
        neededForBoot = true;
      };
      "/var/log" = {
        device = "rpool/var-log";
        fsType = "zfs";
      };
    };
  };
}
