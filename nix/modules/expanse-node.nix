# Entry module imported by the generated /etc/nixos/configuration.nix on
# an installed node. Adds the expanse package (built from the flake
# source baked into the installer) and enables the node module set.
{ config, lib, pkgs, ... }:
{
  imports = [ ./expanse.nix ./md-boot.nix ];

  # The installer passes its own commit, which makes this the ISO's derivation: copied, not compiled.
  # A release tarball's own commit (nix/source-rev) takes precedence once the node is upgraded.
  options.expanse.sourceRev = lib.mkOption {
    type = lib.types.str;
    default = "installer";
    description = "Commit of the flake source this node's expanse is built from.";
  };

  config = {
    nixpkgs.overlays = [
      (final: prev: {
        expanse = final.callPackage ../package.nix {
          version = import ../version.nix;
          rev = import ../source-rev.nix { fallback = config.expanse.sourceRev; };
        };
      })
    ];

    expanse.node.enable = true;

    # nixos-rebuild and the agent's block builds use the nixpkgs this system was built with, which
    # nix/node-system.nix takes from the source's flake.lock; the nixos channel stops mattering.
    # Named "source", the copy is the same store path as the pinned tarball, not a second one.
    nix.nixPath = [ "nixpkgs=${builtins.path { path = pkgs.path; name = "source"; }}" "nixos-config=/etc/nixos/configuration.nix" ];

    # Kernel and a login on the serial port too (VMs, IPMI serial-over-LAN); tty1 stays the main console.
    boot.kernelParams = [ "console=ttyS0,115200n8" "console=tty1" ];

    # An installed node runs the agent with the shipped block catalog and closures (as the VM tests do).
    expanse.agent.enable = lib.mkDefault true;
    expanse.agent.blocksCatalog = lib.mkDefault ../blocks;
    expanse.agent.blocksFlakeRef = lib.mkDefault "/etc/expanse/blocks-flake";
    environment.etc."expanse/blocks-flake".source = ../blocks-flake;

    # The installed node can upgrade from a flake later; keep the store
    # self-contained for now.
    system.stateVersion = "26.05";
  };
}
