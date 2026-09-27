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
    # Expanse well-known ports (internal/config/ports.go): API, raft transport, leases and join
    # between nodes; metrics (7447) and the web UI (8443) for operators. All are TLS-authenticated.
    networking.firewall.allowedTCPPorts = [ 7443 7444 7445 7446 7447 8443 ];
  };
}
