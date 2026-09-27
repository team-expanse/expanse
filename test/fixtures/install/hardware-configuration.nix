# Stands in for nixos-generate-config's output (a QEMU guest) in the node-config-eval check.
{ modulesPath, ... }:
{
  imports = [ (modulesPath + "/profiles/qemu-guest.nix") ];
  boot.initrd.availableKernelModules = [ "ahci" "xhci_pci" "virtio_pci" "virtio_blk" ];
  nixpkgs.hostPlatform = "x86_64-linux";
}
