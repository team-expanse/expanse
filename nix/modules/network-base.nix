# Network base: DHCP, mDNS, hostname.
{ config, pkgs, lib, ... }:
{
  config = lib.mkIf config.expanse.node.enable {
    networking.useDHCP = lib.mkDefault true;
    networking.interfaces = lib.mkDefault { };

    # mDNS so nodes are findable on the LAN.
    services.avahi = {
      enable = true;
      nssmdns4 = true;
      publish = {
        enable = true;
        addresses = true;
      };
    };

    networking.firewall.allowedUDPPorts = [ 5353 ];
  };
}
