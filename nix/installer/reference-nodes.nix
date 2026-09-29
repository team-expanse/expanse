# The configuration.nix the installer writes for each disk layout (its Go goldens), evaluated to a node.
{ self, nixpkgs, system, hardware }:
let
  node = layout: import "${nixpkgs}/nixos" {
    inherit system;
    configuration = builtins.toFile "configuration-${layout}.nix" (builtins.replaceStrings
      [ "@flake@" "@rev@" "./hardware-configuration.nix" ]
      [ "${self}" (self.rev or self.dirtyRev or "dirty") "${hardware}" ]
      (builtins.readFile ../../test/fixtures/install/configuration-${layout}.nix));
  };
in
{
  single = node "single";
  mirror = node "mirror";
}
