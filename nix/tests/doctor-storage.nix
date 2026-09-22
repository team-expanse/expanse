# A5: `expanse doctor storage` against real LVM/DRBD/btrfs state.
#
# One node, `storage-test.nix`'s scratch VG/thin pool for the DRBD-module,
# volume-group and thin-pool rows. The system-mirror row gets its own
# loopback-file btrfs, single-device by construction (storage-test.nix
# already owns /dev/vdb for the scratch VG). The scenario is
# python/doctor_storage_main.py.
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "doctor-storage-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    python3 -c 'import sys; compile(open(sys.argv[1]).read(), sys.argv[1], "exec")' ${./python/doctor_storage_main.py}
    touch $out
  '';
in
{
  name = "expanse-doctor-storage";

  nodes.machine = { ... }: {
    imports = [
      self.nixosModules.expanse
      ../modules/storage-test.nix
    ];
    nixpkgs.overlays = [
      (final: prev: { expanse = self.packages.${prev.system}.expanse; })
    ];
    expanse.node.enable = true;
    expanse.hostId = "01234567";
    expanse.hostname = "doctest";
    expanse.storage-test.enable = true;
    expanse.storage-test.diskSizeMB = 1024;
    expanse.storage-test.poolPercent = 80;
    virtualisation.memorySize = 2048;
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./python/doctor_storage_main.py}
  '';
}
