# Install must refuse to wipe a non-empty disk without --force, and the
# existing filesystem must survive the refusal.
{ self }:
{ pkgs, lib, ... }:
let
  expanse = self.packages.${pkgs.system}.expanse;
  installConfig = pkgs.writeText "expanse-install.yaml" ''
    version: 1
    disks:
      layout: single
      devices: [/dev/vdb]
      force: false
    network:
      mode: dhcp
    cluster:
      mode: none
  '';
in
{
  name = "expanse-install-refuses-dirty-disk";

  nodes.machine = { config, pkgs, ... }: {
    environment.systemPackages = with pkgs; [ expanse disko btrfs-progs e2fsprogs ];
    networking.hostId = "01234567";

    virtualisation.memorySize = 2048;
    virtualisation.cores = 2;
    virtualisation.emptyDiskImages = [ 20480 ];
  };

  testScript = ''
    machine.start()
    machine.wait_for_unit("multi-user.target")

    with subtest("pre-create an ext4 filesystem with data"):
        machine.succeed("mkfs.ext4 -F /dev/vdb")
        machine.succeed("mkdir -p /mnt && mount /dev/vdb /mnt && echo precious > /mnt/precious && umount /mnt")

    machine.succeed("cp ${installConfig} /tmp/install.yaml")

    with subtest("install without --force refuses and preserves the disk"):
        status, out = machine.execute("expanse install --config /tmp/install.yaml 2>&1")
        assert status != 0, "install should have failed on a dirty disk"
        assert "non-empty" in out, f"output should mention non-empty: {out}"
        machine.succeed(
            "mount /dev/vdb /mnt && test \"$(cat /mnt/precious)\" = precious && umount /mnt"
        )
  '';
}
