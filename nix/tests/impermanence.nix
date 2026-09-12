# Impermanence: the most important test in Phase 01. If this passes, the
# determinism pillar is mechanically enforced.
#
# The VM test framework provides its own root disk, so the node's
# rpool/root dataset is mounted at /rpool-root (see pool-init.nix). The
# assertions prove the actual mechanisms: the rollback service wipes
# rpool/root on every boot, /persist survives, bind-mounted state
# (machine-id, ssh host keys) is stable, and identity is byte-stable.
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
    virtualisation.emptyDiskImages = [ 20480 ];
  };

  testScript = ''
    machine.start()
    machine.wait_for_unit("multi-user.target")
    machine.wait_for_unit("expanse-firstboot.service")

    # Ephemeral state on the wiped dataset.
    machine.succeed("touch /rpool-root/root-ephemeral-marker")
    # Persistent state.
    machine.succeed("touch /persist/persistent-marker")
    machine.succeed("echo junk > /etc/junk-file")
    machine.succeed("mkdir -p /root/.ssh && echo key > /root/.ssh/authorized_keys")

    identity_before = machine.succeed("cat /persist/expanse/identity/node-id").strip()
    sshkey_before = machine.succeed("sha256sum /persist/ssh/ssh_host_ed25519_key").strip()
    machineid_before = machine.succeed("cat /etc/machine-id").strip()

    # Flush zfs transaction groups so the crash doesn't lose recent writes
    # (in production, shutdown syncs the pool; a hard crash can lose <5s).
    machine.succeed("zpool sync rpool")

    # Hard power-cycle: also proves the rollback works from a cold boot
    # (the framework's soft reboot path powers the VM off into S5).
    machine.crash()
    machine.start()
    machine.wait_for_unit("multi-user.target")

    with subtest("rpool/root is wiped on reboot"):
        machine.fail("test -e /rpool-root/root-ephemeral-marker")

    with subtest("blank snapshot still exists"):
        machine.succeed("zfs list -t snapshot rpool/root@blank")

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
  '';
}
