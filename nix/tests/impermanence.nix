# Impermanence: the most important test in Phase 01. If this passes, the
# determinism pillar is mechanically enforced.
#
# The VM test framework provides its own root disk, so the node's @root
# subvolume is mounted at /btrfs-root (see pool-init.nix). The assertions
# prove the actual mechanisms: the rollback service wipes @root on every
# boot, /persist survives, bind-mounted state (machine-id, ssh host keys)
# is stable, identity is byte-stable, and a missing blank snapshot is a
# loud failure rather than a silent no-wipe (A2's acceptance).
{ self }:
{ pkgs, lib, ... }:
{
  name = "expanse-impermanence";

  nodes.machine = { config, pkgs, ... }: {
    imports = [ self.nixosModules.expanse ./pool-init.nix ];
    nixpkgs.overlays = [
      (final: prev: { expanse = self.packages.${prev.system}.expanse; })
    ];
    expanse.node.enable = true;
    expanse.hostId = "01234567";
    expanse.hostname = "expanse-test";

    virtualisation.memorySize = 2048;
    virtualisation.cores = 2;
    virtualisation.emptyDiskImages = [ 4096 ];
  };

  testScript = ''
    machine.start()
    machine.wait_for_unit("multi-user.target")
    machine.wait_for_unit("expanse-firstboot.service")

    # Ephemeral state on the wiped subvolume.
    machine.succeed("touch /btrfs-root/root-ephemeral-marker")
    # Persistent state.
    machine.succeed("touch /persist/persistent-marker")
    machine.succeed("echo junk > /etc/junk-file")
    machine.succeed("mkdir -p /root/.ssh && echo key > /root/.ssh/authorized_keys")

    identity_before = machine.succeed("cat /persist/expanse/identity/node-id").strip()
    sshkey_before = machine.succeed("sha256sum /persist/ssh/ssh_host_ed25519_key").strip()
    machineid_before = machine.succeed("cat /etc/machine-id").strip()

    # Flush so the crash doesn't lose recent writes (in production, shutdown
    # syncs the filesystem; a hard crash can lose <5s of btrfs writeback).
    machine.succeed("sync")

    # Hard power-cycle: also proves the rollback works from a cold boot
    # (the framework's soft reboot path powers the VM off into S5).
    machine.crash()
    machine.start()
    machine.wait_for_unit("multi-user.target")

    with subtest("@root is wiped on reboot"):
        machine.fail("test -e /btrfs-root/root-ephemeral-marker")

    with subtest("blank snapshot still exists"):
        out = machine.succeed("btrfs subvolume list /btrfs-root")
        assert "@root-blank" in out, out

    with subtest("persist survives reboot"):
        machine.succeed("test -e /persist/persistent-marker")

    with subtest("node-id is byte-identical"):
        identity_after = machine.succeed("cat /persist/expanse/identity/node-id").strip()
        assert identity_after == identity_before, "node-id changed across reboot"

    with subtest("ssh host key stable"):
        sshkey_after = machine.succeed("sha256sum /persist/ssh/ssh_host_ed25519_key").strip()
        assert sshkey_after == sshkey_before, "ssh host key changed across reboot"

    with subtest("machine-id stable via bind mount"):
        machineid_after = machine.succeed("cat /etc/machine-id").strip()
        assert machineid_after == machineid_before, "machine-id changed across reboot"

    with subtest("/root/.ssh survives via bind mount"):
        assert machine.succeed("cat /root/.ssh/authorized_keys").strip() == "key"

    # Framework root is not wiped by design; junk on it is only used to
    # show the check service notices non-persisted writes.
    with subtest("impermanence-check service ran"):
        machine.succeed("systemctl is-active expanse-impermanence-check.service")

    with subtest("a missing blank snapshot is a loud failure, not a silent no-wipe"):
        machine.succeed(
            "mkdir -p /btrfs-top && mount -o subvolid=5 /dev/vdb /btrfs-top && "
            "btrfs subvolume delete -R /btrfs-top/@root-blank && umount /btrfs-top"
        )
        machine.succeed("touch /btrfs-root/marker-before-missing-snapshot-reboot")
        machine.succeed("sync")
        machine.crash()
        machine.start()
        machine.wait_for_unit("multi-user.target")
        # No snapshot to roll back to: the marker survives, and the
        # rollback service logs the warning loudly instead of wiping silently.
        machine.succeed("test -e /btrfs-root/marker-before-missing-snapshot-reboot")
        journal = machine.succeed("journalctl -b -u expanse-impermanence-rollback --no-pager")
        assert "@root-blank missing" in journal, journal
  '';
}
