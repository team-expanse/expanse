# The installer's live environment, shared by the ISO (iso.nix) and the install-tui VM test.
{ self }:
{ pkgs, lib, ... }:
{
  imports = [ ./installer-tui.nix ];

  # Kernel and a login on the serial port too (VMs, IPMI serial-over-LAN); tty1 stays the main console.
  boot.kernelParams = [ "console=ttyS0,115200n8" "console=tty1" ];

  # The flake source, reachable at a stable path for the installer.
  environment.etc."expanse/flake".source = "${self}";
  environment.variables.EXPANSE_FLAKE = "/etc/expanse/flake";

  # disko and nixos-install evaluate <nixpkgs>; a store path needs no flakes and no network.
  nix.nixPath = lib.mkForce [ "nixpkgs=${pkgs.path}" ];
}
