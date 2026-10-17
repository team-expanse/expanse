# A NixOS development guest for a vm/instance block with cpuModel: host: nix with flakes,
# KVM for nixosTests, and a toolchain for working on Expanse itself (docs/DEVELOPING.md).
{ config, lib, pkgs, modulesPath, ... }:
let
  cfg = config.expanse.devGuest;
in
{
  imports = [ "${modulesPath}/profiles/qemu-guest.nix" ];

  options.expanse.devGuest = {
    user = lib.mkOption {
      type = lib.types.str;
      default = "dev";
      description = "The login user; a member of wheel and kvm.";
    };
    authorizedKeys = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      description = "SSH public keys allowed to log in as the user.";
    };
    extraPackages = lib.mkOption {
      type = lib.types.listOf lib.types.package;
      default = [ ];
      description = "Packages added to the system profile beside the default toolchain.";
    };
  };

  config = {
    system.stateVersion = "26.05";
    networking.hostName = lib.mkDefault "dev";

    # BIOS boot from the raw volume; the console is the block unit's journal on the host.
    boot.loader.grub.device = "/dev/vda";
    boot.kernelParams = [ "console=ttyS0,115200" ];
    boot.growPartition = true;
    fileSystems."/" = { device = "/dev/disk/by-label/nixos"; fsType = "ext4"; autoResize = true; };

    networking.useDHCP = lib.mkDefault true;

    services.openssh.enable = true;
    services.openssh.settings = {
      PasswordAuthentication = false;
      PermitRootLogin = "no";
      AllowUsers = [ cfg.user ];
    };

    users.users.${cfg.user} = {
      isNormalUser = true;
      extraGroups = [ "wheel" "kvm" ];
      openssh.authorizedKeys.keys = cfg.authorizedKeys;
    };
    # Key-only logins leave the user without a password, so sudo cannot ask for one.
    security.sudo.wheelNeedsPassword = false;

    nix.settings = {
      experimental-features = [ "nix-command" "flakes" ];
      system-features = [ "kvm" "nixos-test" "big-parallel" "benchmark" ];
      trusted-users = [ cfg.user ];
    };

    programs.vim = { enable = true; defaultEditor = true; };
    environment.systemPackages = with pkgs; [
      curl git github-cli go pi-coding-agent python3 qemu_kvm screen wget
    ] ++ cfg.extraPackages;
  };
}
