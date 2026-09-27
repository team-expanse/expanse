# Mirror install: `expanse install` onto two blank disks lays down md RAID1 for the ESP
# (metadata 1.0) and the btrfs system. The scenario is python/install_mirror_main.py;
# nixos-install is skipped as in install-unattended.nix, and booting degraded is checked by hand.
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "install-mirror-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    python3 -c 'import sys; compile(open(sys.argv[1]).read(), "testscript", "exec")' ${./python/install_mirror_main.py}
    touch $out
  '';
  installConfig = pkgs.writeText "expanse-install.yaml" ''
    version: 1
    disks:
      layout: mirror
      devices: [/dev/vdb, /dev/vdc]
      force: true
    network:
      interface: auto
      mode: dhcp
    cluster:
      mode: none
    timezone: UTC
  '';
in
{
  name = "expanse-install-mirror";

  nodes.machine = { pkgs, lib, ... }: {
    environment.systemPackages = with pkgs; [ self.packages.${pkgs.system}.expanse disko btrfs-progs lvm2 e2fsprogs util-linux mdadm ];
    environment.etc."expanse/flake".source = self;
    environment.etc."expanse-install.yaml".source = installConfig;
    environment.variables.NIX_PATH = lib.mkForce "nixpkgs=${pkgs.path}";
    boot.swraid.enable = true;
    boot.swraid.mdadmConf = "PROGRAM ${pkgs.coreutils}/bin/true";
    networking.hostId = "01234567";

    virtualisation.memorySize = 2048;
    virtualisation.cores = 2;
    virtualisation.emptyDiskImages = [ 20480 20480 ];

    # disko builds its script inside the offline VM; pre-build it (see install-unattended.nix).
    virtualisation.additionalPaths = [
      ((import "${pkgs.disko}/share/disko" { inherit lib; })._cliDestroyFormatMount
        (import (self + "/nix/installer/disko/mirror.nix") { disks = [ "/dev/vdb" "/dev/vdc" ]; })
        (import pkgs.path { system = pkgs.stdenv.hostPlatform.system; }))
    ];
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./python/install_mirror_main.py}
  '';
}
