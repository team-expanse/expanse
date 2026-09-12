# Mirror layout: 2 disks, rpool mirrored across both. Dataset structure
# is shared with single.nix; only the pool mode differs.
{ ... }@args:
import ./single.nix (removeAttrs args [ "mode" ] // { poolMode = "mirror"; })
