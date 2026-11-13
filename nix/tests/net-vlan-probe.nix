# Probe (Phase 17 N0): do 802.1Q tags cross the test network, can a macvlan on a VLAN device get a
# DHCP lease inside a netns, and does a trunk child still see a VID the host also has a device for?
# Observations only; nothing is asserted.
{ self }:
{ pkgs, lib, ... }:
{
  name = "expanse-net-vlan-probe";
  nodes = {
    # n1: eth1 = management (test vlan 1), eth2 = trunk with native network 192.168.2.0/24.
    n1 = { pkgs, ... }: {
      virtualisation.vlans = [ 1 2 ];
      virtualisation.memorySize = 768;
      environment.systemPackages = [ pkgs.dhcpcd pkgs.tcpdump ];
      # dhcpcd's privilege separation needs its user; the dhcpcd service itself stays off.
      users.users.dhcpcd = { isSystemUser = true; group = "dhcpcd"; };
      users.groups.dhcpcd = { };
    };
    # n2: the switch's far side: DHCP for the native network and VLANs 20 and 30.
    n2 = { ... }: {
      virtualisation.vlans = [ 2 ];
      networking.firewall.enable = false;
      networking.vlans = {
        vl20 = { id = 20; interface = "eth1"; };
        vl30 = { id = 30; interface = "eth1"; };
      };
      networking.interfaces.vl20.ipv4.addresses = [{ address = "10.20.0.1"; prefixLength = 24; }];
      networking.interfaces.vl30.ipv4.addresses = [{ address = "10.30.0.1"; prefixLength = 24; }];
      services.dnsmasq = {
        enable = true;
        resolveLocalQueries = false;
        settings = {
          port = 0;
          interface = [ "eth1" "vl20" "vl30" ];
          bind-dynamic = true;
          dhcp-range = [
            "192.168.2.100,192.168.2.150,1h"
            "10.20.0.100,10.20.0.150,1h"
            "10.30.0.100,10.30.0.150,1h"
          ];
          log-dhcp = true;
        };
      };
    };
  };
  testScript = builtins.readFile ./net-vlan-probe.py;
}
