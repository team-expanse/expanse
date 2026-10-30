# Blocks start after, and so stop before, network-online.target. Static addresses aren't ordered
# against it, so at shutdown they'd go while a VM still saves its RAM and DRBD still syncs.
{ config, lib, ... }:
{
  config = lib.mkIf (config.expanse.agent.enable && !config.networking.useNetworkd) {
    systemd.services = lib.mapAttrs'
      (name: _: lib.nameValuePair "network-addresses-${name}" { before = [ "network-online.target" ]; })
      config.networking.interfaces;
  };
}
