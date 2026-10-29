# Phase 16 V4: a running VM takes new CPU and memory requests live, within its limits, and keeps
# its new size through a suspend and resume. vm-instance.nix's cluster and guest, with room for the save.
{ self }:
import ./vm-instance.nix {
  inherit self;
  name = "vm-live-resize";
  script = ./python/vm_live_resize.py;
  diskSize = 4096;
}
