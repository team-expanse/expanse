# A NixOS guest for a vm/instance block that runs Pando (https://github.com/trypando/pando)
# under Docker. Its container images are baked in, so the first boot needs no registry.
{ config, lib, pkgs, modulesPath, ... }:
let
  images = import ./images.nix { inherit pkgs; };
  stateDir = "/var/lib/pando-guest";
  cfg = config.expanse.pandoGuest;
  envFile = pkgs.writeText "pando.env"
    (lib.concatStrings (lib.mapAttrsToList (k: v: "${k}=${v}\n") cfg.settings));
in
{
  imports = [ "${modulesPath}/profiles/qemu-guest.nix" ];

  options.expanse.pandoGuest = {
    authorizedKeys = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      description = "SSH public keys allowed to log in as root.";
    };
    settings = lib.mkOption {
      type = lib.types.attrsOf lib.types.str;
      default = { };
      example = { PANDO_ADMIN_PASSWORD = "a-long-first-password"; };
      description = ''
        Variables for Pando's compose file (Pando's README lists them), seeded once into
        /var/lib/pando-guest/pando.env; edit that copy afterwards and restart pando.service.
      '';
    };
  };

  config = {
    system.stateVersion = "26.05";
    networking.hostName = lib.mkDefault "pando";

    # BIOS boot from the raw volume; the console is the block unit's journal on the host.
    boot.loader.grub.device = "/dev/vda";
    boot.kernelParams = [ "console=ttyS0,115200" ];
    boot.growPartition = true;
    fileSystems."/" = { device = "/dev/disk/by-label/nixos"; fsType = "ext4"; autoResize = true; };

    networking.useDHCP = lib.mkDefault true;
    networking.firewall.allowedTCPPorts = [ 22 8080 ] ++ lib.range 9000 9019;

    services.openssh.enable = true;
    services.openssh.settings.PasswordAuthentication = false;
    users.users.root.openssh.authorizedKeys.keys = cfg.authorizedKeys;

    virtualisation.docker.enable = true;
    environment.systemPackages = [ pkgs.docker ];

    # Seeds the editable settings once, loads any missing baked-in image, starts the stack, waits for
    # Pando to listen, then syncs, so the guest reports ready only once first-run state is on disk.
    systemd.services.pando = {
      description = "Pando under Docker Compose";
      wantedBy = [ "multi-user.target" ];
      after = [ "docker.service" "network-online.target" ];
      requires = [ "docker.service" ];
      wants = [ "network-online.target" ];
      path = [ pkgs.docker pkgs.curl ];
      serviceConfig = { Type = "oneshot"; RemainAfterExit = true; TimeoutStartSec = "15min"; };
      script = ''
        mkdir -p ${stateDir}
        [ -e ${stateDir}/pando.env ] || install -m 0600 ${envFile} ${stateDir}/pando.env
        ${lib.concatMapStrings (i: ''
          docker image inspect ${i.ref} >/dev/null 2>&1 || docker load -i ${i.tarball}
        '') images}
        docker compose -p pando -f ${./compose.yaml} --env-file ${stateDir}/pando.env up -d --wait
        # Pando writes secrets.key and token.key without fsync just before it listens; compose has no
        # health check to wait for that, and a crash before the sync leaves them empty for good.
        until curl -s -o /dev/null http://127.0.0.1:8080/; do sleep 2; done
        sync
      '';
    };

    # Prints the guest's address, or that Pando failed, on the serial console (the host's journal).
    systemd.services.pando-address = {
      wantedBy = [ "multi-user.target" ];
      after = [ "pando.service" ];
      serviceConfig = { Type = "oneshot"; StandardOutput = "tty"; TTYPath = "/dev/ttyS0"; };
      script = ''
        if ! ${pkgs.systemd}/bin/systemctl is-active --quiet pando.service; then
          echo "pando-guest: Pando failed to start; run journalctl -u pando in the guest"
          exit 0
        fi
        echo "pando-guest: console at http://$(${pkgs.iproute2}/bin/ip -4 -o addr show scope global | ${pkgs.gawk}/bin/awk '{sub("/.*","",$4); print $4; exit}'):8080"
      '';
    };
  };
}
