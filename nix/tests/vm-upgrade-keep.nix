# Phase 16 V5: a switch to another QEMU flags the running VM, `expanse ctl vm update` moves it onto
# that QEMU, and a node reboot saves and resumes it. vm-instance.nix's cluster and guest.
{ self }:
import ./vm-instance.nix {
  inherit self;
  name = "vm-upgrade-keep";
  script = ./python/vm_upgrade_keep.py;
  diskSize = 4096;
  nodeModule = ./vm-upgrade-keep-node.nix;
}
