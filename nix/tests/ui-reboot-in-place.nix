# Upgrades with "Reboot nodes that need it" on a one-node cluster: it reboots in place, its
# replicas held, and comes back on the release.
{ self }:
import ./rolling-reboot.nix {
  inherit self;
  name = "expanse-ui-reboot-in-place";
  size = 1;
  script = ./python/ui_reboot_in_place.py;
}
