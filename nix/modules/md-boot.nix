# Booting from md RAID1 (the mirror layout): bootctl refuses an ESP that is not a GPT
# partition, so the bootloader installer runs with its ESP checks relaxed.
{ config, lib, pkgs, extendModules, ... }:
let
  cfg = config.expanse.node;
  bootDevice = config.fileSystems."/boot".device or "";
  # The systemd-boot installer as this config builds it, minus the wrapper below.
  stockInstaller = (extendModules {
    modules = [{ expanse.node.relaxEspInstaller = lib.mkForce false; }];
  }).config.system.build.installBootLoader;
  # mdmonitor reports array events (a failed or missing disk) to the journal.
  mdEvent = pkgs.writeShellScript "md-event" ''
    ${pkgs.util-linux}/bin/logger -t mdadm -p daemon.warning "md event: $*"
  '';
in
{
  options.expanse.node = {
    mdEsp = lib.mkOption {
      type = lib.types.bool;
      default = lib.hasPrefix "/dev/md" bootDevice;
      defaultText = lib.literalExpression ''/boot is on an md array'';
      description = "Whether /boot is an md RAID1 array (metadata 1.0) that bootctl must accept.";
    };
    relaxEspInstaller = lib.mkOption {
      type = lib.types.bool;
      default = true;
      internal = true;
      description = "Wrap the bootloader installer when mdEsp; off only to build the stock installer.";
    };
  };

  config = lib.mkMerge [
    (lib.mkIf config.boot.swraid.enable {
      boot.swraid.mdadmConf = "PROGRAM ${mdEvent}";
      environment.systemPackages = [ pkgs.gptfdisk ]; # sgdisk, to partition a replacement disk
    })
    (lib.mkIf cfg.mdEsp {
      # An NVRAM entry needs the ESP's partition UUID, which an md array lacks; firmware uses each disk's fallback path.
      boot.loader.efi.canTouchEfiVariables = lib.mkForce false;
    })
    (lib.mkIf (cfg.mdEsp && cfg.relaxEspInstaller) {
      system.build.installBootLoader = lib.mkForce (pkgs.writeShellScript "install-bootloader-relaxed-esp" ''
        export SYSTEMD_RELAX_ESP_CHECKS=1
        exec ${stockInstaller} "$@"
      '');
    })
  ];
}
