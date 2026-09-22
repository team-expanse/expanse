# btrfs subvolume mounts for an installed Expanse node. Device, fsType and
# mount options for each subvolume are declared by disko's layout file
# (nix/installer/disko/{single,mirror}.nix), which writeConfiguration
# imports alongside this module; this only adds what disko's declaration
# does not -- which of them must be mounted before switch_root, which the
# initrd services in impermanence.nix depend on.
{ config, lib, ... }:
{
  config = lib.mkIf config.expanse.node.enable {
    fileSystems."/".neededForBoot = true;
    fileSystems."/nix".neededForBoot = true;
    fileSystems."/persist".neededForBoot = true;
  };
}
