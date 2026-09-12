# Raidz1 layout: 3+ disks, rpool in raidz1. Dataset structure is shared
# with single.nix; only the pool mode differs.
{ ... }@args:
import ./single.nix (removeAttrs args [ "mode" ] // { poolMode = "raidz1"; })
