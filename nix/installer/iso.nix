# Expanse installer ISO: a live environment that can turn any
# x86_64/aarch64 machine into an Expanse node.
{ self, nixpkgs, pkgs, lib, ... }:
let
  # The flake source is baked into the ISO so the installer can evaluate
  # the node configuration offline.
  flakeSource = self;
  # Installed nodes share almost all of these; nixos-install copies them off the ISO instead of downloading.
  referenceNodes = import ./reference-nodes.nix {
    inherit self nixpkgs;
    inherit (pkgs.stdenv.hostPlatform) system;
    hardware = builtins.toFile "hardware-configuration.nix" ''
      { nixpkgs.hostPlatform = "${pkgs.stdenv.hostPlatform.system}"; }
    '';
  };
in
{
  imports = [
    "${nixpkgs}/nixos/modules/installer/cd-dvd/installation-cd-minimal.nix"
    ../modules/expanse.nix
    (import ./live.nix { inherit self; })
  ];

  expanse.node.enable = lib.mkForce false; # base node modules are for installed systems

  # The installer runs the flake's own expanse, so it reports the release it installs.
  nixpkgs.overlays = [
    (final: prev: { expanse = self.packages.${final.stdenv.hostPlatform.system}.expanse; })
  ];
  image.baseName = lib.mkForce "expanse-${pkgs.expanse.version}-${pkgs.stdenv.hostPlatform.system}";

  environment.pathsToLink = [ "/share/licenses" ]; # expanse's licence and third-party notices
  environment.systemPackages = with pkgs; [
    expanse
    disko
    btrfs-progs
    lvm2
  ];

  networking.hostId = "00000000"; # installer-only, never imports a real pool

  hardware.enableRedistributableFirmware = true;

  # The installation-device profile enables NetworkManager (and with it
  # ModemManager, libqmi, ...). We use plain DHCP — size discipline.
  networking.networkmanager.enable = lib.mkForce false;
  networking.wireless.enable = lib.mkForce false;

  # SSH for headless installs: random root password printed on console.
  services.openssh = {
    enable = true;
    settings.PasswordAuthentication = true;
  };
  systemd.services.expanse-installer-ssh-password = {
    description = "Generate and print a random root password for headless installs";
    before = [ "sshd.service" ];
    wantedBy = [ "multi-user.target" ];
    serviceConfig.Type = "oneshot";
    path = [ pkgs.openssl pkgs.coreutils ];
    script = ''
      pw=$(openssl rand -base64 18)
      echo "root:$pw" | chpasswd
      echo ""
      echo "=========================================================="
      echo " Expanse installer — headless SSH access:"
      echo "   ssh root@<this-machine-ip>"
      echo "   password: $pw"
      echo "=========================================================="
    '';
  };

  # mDNS advertisement so installers are findable on the LAN.
  services.avahi = {
    enable = true;
    nssmdns4 = true;
    publish = {
      enable = true;
      addresses = true;
    };
    extraServiceFiles = {
      "expanse-installer" =
        ''<?xml version="1.0" standalone='no'?>
          <!DOCTYPE service-group SYSTEM "avahi-service.dtd">
          <service-group>
            <name replace-wildcards="yes">Expanse Installer on %h</name>
            <service>
              <type>_expanse-installer._tcp</type>
              <port>22</port>
            </service>
          </service-group>
        '';
    };
  };

  # Bake the flake source into the store for offline evaluation, and the reference nodes for a fast install.
  isoImage.storeContents = [ flakeSource ]
    ++ map (node: node.config.system.build.toplevel) (lib.attrValues referenceNodes);

  # Size control.
  isoImage.squashfsCompression = "zstd -Xcompression-level 19";
  isoImage.makeEfiBootable = true;
  isoImage.makeUsbBootable = true;
}
