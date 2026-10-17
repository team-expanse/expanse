# 1.10.1: an installed node runs dhcpcd on every interface. A VM's macvtap child carries the guest's MAC,
# so dhcpcd used to lease the guest's reservation to the node itself. No guest is needed to show it:
# the child is made as runVM makes it. The scenario is python/net_macvtap_dhcp.py.
{ self }:
{ pkgs, lib, ... }:
{
  name = "expanse-net-macvtap-dhcp";

  nodes.n1 = { ... }: {
    imports = [ self.nixosModules.expanse ];
    nixpkgs.overlays = [
      (final: prev: { expanse = self.packages.${prev.system}.expanse; })
    ];
    expanse.node.enable = true;
    expanse.hostId = "00000001";
    expanse.hostname = "n1";
    # As installed: DHCP on every interface (the test framework turns it off by default).
    networking.useDHCP = lib.mkForce true;
  };

  nodes.router = { ... }: {
    networking.firewall.allowedUDPPorts = [ 67 ];
    services.dnsmasq = {
      enable = true;
      resolveLocalQueries = false;
      settings = {
        port = 0;
        interface = "eth1";
        dhcp-range = [ "192.168.1.100,192.168.1.150,1h" ];
        dhcp-host = [ "02:00:00:00:de:01,192.168.1.60" ];
      };
    };
  };

  testScript = builtins.readFile ./python/net_macvtap_dhcp.py;
}
