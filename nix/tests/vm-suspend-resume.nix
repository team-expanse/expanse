# Phase 16 V2: suspend saves a VM's RAM on its node and resume continues the same boot; a VM
# moved while suspended boots fresh. vm-instance.nix's cluster and guest, with room for the save.
{ self }:
import ./vm-instance.nix {
  inherit self;
  name = "vm-suspend-resume";
  script = ./python/vm_suspend_resume.py;
  diskSize = 4096;
}
