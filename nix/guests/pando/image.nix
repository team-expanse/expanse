# A raw, BIOS-bootable disk image of the Pando guest, to be written onto a vm/instance volume.
{ nixpkgs, system, modules ? [ ] }:
let
  guest = nixpkgs.lib.nixosSystem {
    inherit system;
    modules = [ ./module.nix ] ++ modules;
  };
in
import "${nixpkgs}/nixos/lib/make-disk-image.nix" {
  inherit (guest) config pkgs;
  inherit (nixpkgs) lib;
  format = "raw";
  partitionTableType = "legacy";
  diskSize = "auto";
  additionalSpace = "2048M";
}
