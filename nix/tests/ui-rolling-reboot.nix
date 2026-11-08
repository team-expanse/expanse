# Upgrades with "Reboot nodes that need it" on a 3-node cluster: each node drains, reboots and
# goes back in service in turn; a replica with nowhere else to run stays through the reboot.
{ self }:
import ./rolling-reboot.nix {
  inherit self;
  name = "expanse-ui-rolling-reboot";
  size = 3;
  script = ./python/ui_rolling_reboot.py;
}
