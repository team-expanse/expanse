{ config, pkgs, ... }:
{
  # Partitioning was done by disko at install time; the same layout declares the mounts.
  imports = [
    ./hardware-configuration.nix
    @flake@/nix/modules/expanse-node.nix
    (import @flake@/nix/modules/disk-layout.nix {
      layout = @flake@/nix/installer/disko/single.nix;
      disks = [ "/dev/vda" ];
    })
  ];
  expanse.hostId = "00000000";
  expanse.hostname = "golden-node";
  expanse.ssh.authorizedKeys = [ "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIgolden golden@test" ];
  expanse.disks = [ "/dev/vda" ];
  expanse.sourceRev = "@rev@";
}
