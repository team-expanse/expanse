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
    backup = {
      enable = lib.mkEnableOption "restic backups of the guest's home directories and /etc/nixos";
      repository = lib.mkOption {
        type = lib.types.str;
        example = "rest:http://nas.lan:8000/dev";
        description = "The restic repository; it is created on the first run.";
      };
      passwordFile = lib.mkOption {
        type = lib.types.str;
        example = "/var/lib/restic/password";
        description = "Path in the guest (not the nix store) of the repository password.";
      };
      environmentFile = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        description = "Environment file with repository credentials, e.g. for S3 or a REST server login.";
      };
      paths = lib.mkOption {
        type = lib.types.listOf lib.types.str;
        default = [ "/home" "/etc/nixos" ];
        description = "What is backed up; /home covers each user's repos and ~/.ssh.";
      };
      exclude = lib.mkOption {
        type = lib.types.listOf lib.types.str;
        default = [ "/home/*/.cache" ];
        description = "Patterns left out of the backup.";
      };
      schedule = lib.mkOption {
        type = lib.types.str;
        default = "daily";
        description = "systemd OnCalendar expression; a run missed while the guest was off starts at boot.";
      };
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

    # `restic-dev` is the restore tool: the module wraps restic with this repository and password.
    services.restic.backups.dev = lib.mkIf cfg.backup.enable {
      inherit (cfg.backup) repository passwordFile environmentFile paths exclude;
      initialize = true;
      timerConfig = { OnCalendar = cfg.backup.schedule; Persistent = true; RandomizedDelaySec = "1h"; };
      pruneOpts = [ "--keep-daily 7" "--keep-weekly 5" "--keep-monthly 12" ];
    };
    # A missing path fails the run, and /etc/nixos only exists once someone writes a config there.
    systemd.tmpfiles.rules = lib.optional cfg.backup.enable "d /etc/nixos 0755 root root -";
  };
}
