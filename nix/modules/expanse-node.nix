# Entry module imported by the generated /etc/nixos/configuration.nix on
# an installed node. Adds the expanse package (built from the flake
# source baked into the installer) and enables the node module set.
{ ... }:
{
  imports = [ ./expanse.nix ];

  nixpkgs.overlays = [
    (final: prev: {
      expanse = final.callPackage ../package.nix {
        version = import ../version.nix;
        rev = "installer";
      };
    })
  ];

  expanse.node.enable = true;

  # The installed node can upgrade from a flake later; keep the store
  # self-contained for now.
  system.stateVersion = "26.05";
}
