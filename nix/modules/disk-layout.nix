# An installed node's mounts from its install-time disko layout, via the devices._config
# mapping disko's own NixOS module merges in (nixpkgs' disko package, so no extra input).
{ layout, disks }:
{ pkgs, lib, ... }:
let
  disko = import "${pkgs.disko}/share/disko" { inherit lib; };
  cfg = disko.config (import layout { inherit disks; });
in
{
  # Named keys, not the whole attrset: pkgs must not wait on this module's config.
  boot = cfg.boot;
  fileSystems = cfg.fileSystems;
  swapDevices = cfg.swapDevices;
  # Keeps the disko source on the node, so it can re-evaluate this module offline.
  environment.systemPackages = [ pkgs.disko ];
}
