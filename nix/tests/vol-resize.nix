# §6 vol-resize (G6.14): a 10 GiB volume with a mounted ext4 filesystem
# under fio load, grown online to 20 GiB with no unmount — resize2fs
# must succeed online, no I/O errors may occur during the operation,
# and data already on the filesystem (fio's own crc32c-verified file,
# plus a fresh write into the newly grown space) must be intact.
{ self }:
{ pkgs, lib, ... }:
let
  nodeCommon = idx: {
    imports = [
      self.nixosModules.expanse
      ../modules/storage-test.nix
    ];
    nixpkgs.overlays = [
      (final: prev: { expanse = self.packages.${prev.system}.expanse; })
    ];
    expanse.node.enable = true;
    expanse.agent.enable = true;
    expanse.hostId = "0000000${toString idx}";
    expanse.hostname = "n${toString idx}";
    expanse.agent.exvolPool = "volumes";
    expanse.storage-test.enable = true;
    # 10 GiB volume + headroom for the resize to 20 GiB on every replica.
    expanse.storage-test.poolSizeMB = 24576;
    expanse.agent.raftAdvertise = "192.168.1.${toString idx}:7444";
    boot.kernelModules = [ "nbd" ];
    virtualisation.memorySize = 2048;
    virtualisation.diskSize = 30 * 1024;
    networking.firewall.interfaces.exp0.allowedTCPPorts = [ 9440 ];
    environment.systemPackages = with pkgs; [ zfs nbd python3 e2fsprogs fio ];
  };
  lint = pkgs.runCommand "vol-resize-lint" { } ''
    ${pkgs.python3}/bin/python3 -m py_compile ${./python/vol_resize_main.py}
    cat ${./cluster-common.py} ${./python/vol_resize_main.py} | ${pkgs.python3}/bin/python3 -c 'import sys; compile(sys.stdin.read(), "testscript", "exec")'
    touch $out
  '';
in
{
  name = "expanse-vol-resize";

  nodes = {
    n1 = { ... }: nodeCommon 1;
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./python/vol_resize_main.py}
  '';
}
