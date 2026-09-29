# Expanse base system: kernel, locale, time, users, ssh, packages.
# Everything here is multiplied across every node — footprint discipline.
{ config, pkgs, lib, ... }:
{
  config = lib.mkIf config.expanse.node.enable {
    # Pinned to the LTS series, not "latest": DRBD 9 is an out-of-tree
    # module built against the running kernel, and does not yet compile
    # against the latest series (C2d's finding). btrfs and LVM are
    # in-tree, so this is the only kernel constraint left.
    boot.kernelPackages = pkgs.linuxPackages;

    boot.loader.systemd-boot.enable = true;
    boot.loader.efi.canTouchEfiVariables = true;

    # networking.hostId is set by identity.nix from the node UUID.

    # Clock sync matters — Raft leases depend on it.
    services.chrony.enable = true;
    services.chrony.extraConfig = lib.mkAfter "makestep 1.0 3";

    # Cluster-wide consistency; display TZ is a UI concern.
    time.timeZone = lib.mkDefault "UTC";
    i18n.defaultLocale = "en_US.UTF-8";

    users.users.expanse = {
      uid = 990;
      isSystemUser = true;
      group = "expanse";
      home = "/persist/expanse";
      createHome = false; # created by firstboot with correct modes
    };
    users.groups.expanse = { };

    users.users.root = {
      # root login disabled except via console; SSH key auth only.
      hashedPassword = "!";
      openssh.authorizedKeys.keys = config.expanse.ssh.authorizedKeys;
    };

    services.openssh = {
      enable = true;
      settings = {
        PasswordAuthentication = false;
        PermitRootLogin = "prohibit-password";
      };
      hostKeys = [
        { path = "/persist/ssh/ssh_host_ed25519_key"; type = "ed25519"; }
        { path = "/persist/ssh/ssh_host_rsa_key"; type = "rsa"; }
      ];
    };

    # Package licences (expanse ships its own and its vendored modules') under /run/current-system/sw/share/licenses.
    environment.pathsToLink = [ "/share/licenses" ];

    environment.systemPackages = with pkgs; [
      btrfs-progs
      smartmontools
      pciutils
      usbutils
      lm_sensors
      iproute2
      nftables
      wireguard-tools
      tmux
      vim
      curl
      htop
      expanse
    ];

    documentation.enable = false;
    documentation.nixos.enable = false;

    nix.settings.auto-optimise-store = true;
    nix.gc = {
      automatic = true;
      dates = "weekly";
      options = "--delete-older-than 7d";
    };
  };
}
