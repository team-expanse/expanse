# An installed node's system, built with the nixpkgs its expanse source pins (not the nixos channel).
# `nix-build /etc/nixos/expanse/nix/node-system.nix -A config.system.build.toplevel`
{ configuration ? /etc/nixos/configuration.nix, system ? builtins.currentSystem }:
import "${import ./node-nixpkgs.nix}/nixos" { inherit configuration system; }
