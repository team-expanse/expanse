# Entry module imported by the generated /etc/nixos/configuration.nix on
# an installed node. Adds the expanse package (built from the flake
# source baked into the installer) and enables the node module set.
{ lib, ... }:
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
}
