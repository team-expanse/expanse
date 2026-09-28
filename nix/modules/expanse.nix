# Top-level Expanse NixOS module: options.expanse.* and imports.
{ config, pkgs, lib, ... }:
{
  imports = [
    ./base.nix
    ./storage.nix
    ./impermanence.nix
    ./identity.nix
    ./network-base.nix
    ./hardening.nix
    ./agent.nix
    ./console.nix
  ];

  options = {
    expanse.node.enable = lib.mkEnableOption "Expanse node base system";

    expanse.hostId = lib.mkOption {
      type = lib.types.strMatching "[0-9a-f]{8}";
      default = "00000000";
      description = "NixOS networking.hostId; first 8 hex chars of the node-id. Set at install time.";
    };

    expanse.hostname = lib.mkOption {
      type = lib.types.str;
      default = "expanse";
      description = "Node hostname; default expanse-<node-id prefix> set at install time.";
    };

    expanse.ssh.authorizedKeys = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      description = "Operator SSH public keys allowed for root.";
    };

    expanse.disks = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      description = "Target disk devices recorded at install time (informational).";
    };

    expanse.persistDir = lib.mkOption {
      type = lib.types.str;
      default = "/persist";
      description = "Persistent state directory (the @persist btrfs subvolume).";
    };
  };

  config = lib.mkIf config.expanse.node.enable {
    networking.hostId = config.expanse.hostId;
    networking.hostName = config.expanse.hostname;
    environment.etc."expanse/persist-dir".text = config.expanse.persistDir;
  };
}
