# A power cut right after boot keeps /var/lib/nixos's user maps intact (the 1.16.1 boot-time failure).
# /persist commits every 300 s here, so only an explicit sync can make the maps durable before the crash.
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "persist-crash-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    python3 -c 'import sys; compile(open(sys.argv[1]).read(), "testscript", "exec")' ${./python/persist_crash.py}
    touch $out
  '';
in
{
  name = "expanse-persist-crash";

  nodes.machine = { ... }: {
    imports = [ self.nixosModules.expanse ./pool-init.nix ];
    nixpkgs.overlays = [
      (final: prev: { expanse = self.packages.${prev.system}.expanse; })
    ];
    expanse.node.enable = true;
    expanse.hostId = "01234567";
    virtualisation.memorySize = 2048;
    virtualisation.emptyDiskImages = [ 20480 ];
    virtualisation.fileSystems."/persist".options = lib.mkForce [ "subvol=@persist" "commit=300" ];
    environment.systemPackages = [ pkgs.python3 ];

    # Reports, before activation rewrites it, whether the last boot's uid-map reached the disk.
    boot.initrd.systemd.services.persist-crash-probe = {
      wantedBy = [ "initrd.target" ];
      after = [ "sysroot-persist.mount" ];
      before = [ "initrd.target" ];
      unitConfig = {
        DefaultDependencies = "no";
        RequiresMountsFor = [ "/sysroot/persist" ];
      };
      serviceConfig.Type = "oneshot";
      path = [ pkgs.coreutils ];
      script = ''
        if [ "$(head -c 1 /sysroot/persist/var/lib/nixos/uid-map 2>/dev/null)" = "{" ]; then
          echo "persist-crash: uid-map kept"
        else
          echo "persist-crash: uid-map lost"
        fi
      '';
    };
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./python/persist_crash.py}
  '';
}
