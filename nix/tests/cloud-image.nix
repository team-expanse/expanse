# A small cloud image for ui-vm-cloud-image: NixOS with cloud-init (NoCloud), DHCP and sshd, as a
# compressed qcow2 like the distributions publish. cloud-init sets the hostname, user and keys.
{ nixpkgs, system }:
let
  guest = nixpkgs.lib.nixosSystem {
    inherit system;
    modules = [
      ({ modulesPath, pkgs, lib, ... }: {
        imports = [ "${modulesPath}/profiles/qemu-guest.nix" ];
        system.stateVersion = "26.05";
        boot.loader.grub.device = "/dev/vda";
        # Nested under the test VM, every byte on the guest's serial console and every GRUB
        # framebuffer write traps through two hypervisors (~400 B/s), so boot quietly and at once.
        boot.kernelParams = [ "console=ttyS0,115200" "quiet" "systemd.show_status=false" "udev.log_level=3" ];
        boot.loader.timeout = 0;
        boot.loader.grub.splashImage = null;
        boot.loader.grub.extraConfig = "terminal_output console";
        boot.growPartition = true;
        # The initrd's usual 90 s for the root device is too short nested; a missing disk still fails.
        fileSystems."/" = {
          device = "/dev/disk/by-label/nixos";
          fsType = "ext4";
          autoResize = true;
          options = [ "x-systemd.device-timeout=900s" ];
        };
        documentation.enable = false;
        networking.hostName = "";
        networking.useDHCP = true;
        networking.firewall.enable = false;
        services.cloud-init.enable = true;
        # cloud-init's units log to the console too; keep that off the slow serial line.
        systemd.services = lib.genAttrs [ "cloud-init-local" "cloud-init" "cloud-config" "cloud-final" ]
          (_: { serviceConfig.StandardOutput = lib.mkForce "journal"; });
        services.openssh.enable = true;
        # The users the seed creates log in with /bin/bash, as on the distributions' images.
        systemd.tmpfiles.rules = [ "L+ /bin/bash - - - - ${pkgs.bashInteractive}/bin/bash" ];
      })
    ];
  };
in
import "${nixpkgs}/nixos/lib/make-disk-image.nix" {
  inherit (guest) config pkgs;
  inherit (nixpkgs) lib;
  format = "qcow2-compressed";
  partitionTableType = "legacy";
  diskSize = "auto";
  additionalSpace = "512M";
}
