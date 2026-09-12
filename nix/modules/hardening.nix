# Hardening baseline (minimum for Phase 01; full treatment in Phase 14).
{ config, pkgs, lib, ... }:
{
  config = lib.mkIf config.expanse.node.enable {
    services.openssh.settings = {
      PasswordAuthentication = false;
      PermitRootLogin = "prohibit-password";
      KexAlgorithms = [
        "curve25519-sha256"
        "curve25519-sha256@libssh.org"
        "diffie-hellman-group16-sha512"
      ];
      Ciphers = [ "chacha20-poly1305@openssh.com" "aes256-gcm@openssh.com" ];
    };

    networking.nftables.enable = true;
    networking.firewall = {
      enable = true;
      # default-deny inbound; allow ssh, mDNS, ICMP.
      allowedTCPPorts = [ 22 ];
      allowedUDPPorts = [ 5353 ];
      allowPing = true;
    };

    security.sudo.wheelNeedsPassword = true;

    boot.kernel.sysctl = {
      # IP forwarding is enabled in Phase 05.
      "net.ipv4.ip_forward" = 0;
      "net.ipv6.conf.all.forwarding" = 0;
      "kernel.dmesg_restrict" = 1;
      "kernel.kptr_restrict" = 2;
    };

    # No X, no printing, no bluetooth.
    services.xserver.enable = false;
    services.printing.enable = false;
    hardware.bluetooth.enable = false;
  };
}
