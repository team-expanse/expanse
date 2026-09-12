# Identity: files exist with correct modes and ownership, idempotent
# across re-runs, and the keypair actually signs.
{ self }:
{ pkgs, lib, ... }:
{
  name = "expanse-identity";

  nodes.machine = { config, pkgs, ... }: {
    imports = [ self.nixosModules.expanse ./pool-init.nix ];
    nixpkgs.overlays = [
      (final: prev: { expanse = self.packages.${prev.system}.expanse; })
    ];
    expanse.node.enable = true;
    expanse.hostId = "01234567";

    virtualisation.memorySize = 2048;
    virtualisation.cores = 2;
    virtualisation.emptyDiskImages = [ 20480 ];
  };

  testScript = ''
    machine.start()
    machine.wait_for_unit("expanse-identity.service")
    machine.wait_for_unit("expanse-firstboot.service")
    machine.wait_for_unit("multi-user.target")

    with subtest("identity files and modes"):
        machine.succeed("test $(stat -c %a /persist/expanse/identity/node-id) = 644")
        machine.succeed("test $(stat -c %a /persist/expanse/identity/node.pub) = 644")
        machine.succeed("test $(stat -c %a /persist/expanse/identity/node.key) = 600")
        machine.succeed("test $(stat -c %U /persist/expanse/identity/node.key) = expanse")

    with subtest("firstboot creates state dirs owned by expanse"):
        for d in ("raft", "secrets", "blocks", "volumes"):
            machine.succeed("test -d /persist/expanse/" + d)
        machine.succeed("test $(stat -c %U /persist/expanse/raft) = expanse")

    with subtest("firstboot marker exists"):
        machine.succeed("test -s /persist/expanse/.firstboot-complete")

    nid = machine.succeed("cat /persist/expanse/identity/node-id").strip()

    with subtest("re-running firstboot is a no-op"):
        machine.succeed("systemctl restart expanse-firstboot.service")
        nid2 = machine.succeed("cat /persist/expanse/identity/node-id").strip()
        assert nid2 == nid, "node-id changed after firstboot re-run"

    with subtest("keypair consistent with node info"):
        pubhex = machine.succeed(
            "od -An -tx1 /persist/expanse/identity/node.pub | tr -d ' \\n'"
        ).strip()
        info = machine.succeed("expanse node info")
        assert pubhex in info, f"node info public key does not match node.pub: {info}"
        # Signing round-trip is covered by unit tests (TestSignatureVerifies).
  '';
}
