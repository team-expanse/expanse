# Interactive install: the ISO's tty1 installer TUI, driven by keystrokes the way a person
# would, partitions a blank disk and reaches its done screen. The scenario is
# python/install_tui_main.py; nixos-install is skipped as in install-unattended.nix.
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "install-tui-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    python3 -c 'import sys; compile(open(sys.argv[1]).read(), "testscript", "exec")' ${./python/install_tui_main.py}
    touch $out
  '';
in
{
  name = "expanse-install-tui";

  nodes.machine = { pkgs, lib, ... }: {
    imports = [ (import (self + "/nix/installer/live.nix") { inherit self; }) ];
    expanse.installer.tuiArgs = "--skip-system-install";

    environment.systemPackages = with pkgs; [ self.packages.${pkgs.system}.expanse disko btrfs-progs lvm2 e2fsprogs util-linux ];
    networking.hostId = "01234567";

    virtualisation.memorySize = 2048;
    virtualisation.cores = 2;
    virtualisation.emptyDiskImages = [ 20480 ];

    # disko builds its script inside the offline VM; pre-build it (see install-unattended.nix).
    virtualisation.additionalPaths = [
      ((import "${pkgs.disko}/share/disko" { inherit lib; })._cliDestroyFormatMount
        (import (self + "/nix/installer/disko/single.nix") { disks = [ "/dev/vdb" ]; })
        (import pkgs.path { system = pkgs.stdenv.hostPlatform.system; }))
    ];
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./python/install_tui_main.py}
  '';
}
