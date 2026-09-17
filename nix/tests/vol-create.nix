# §6 vol-create: first end-to-end Phase 06 VM test (G6.1). Wires T01–T09:
# a 10 GiB R=3 volume is created via `expanse ctl volume create` on the
# leader; the per-node volume runtimes converge — 3 zvols (one per
# node), the primary runs the coordinator + NBD device, and 1 GiB of
# random data round-trips through /dev/exvol/<id> with a matching
# sha256.
{ self }:
{ pkgs, lib, ... }:
let
  expanse = self.packages.${pkgs.system}.expanse;
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
    expanse.storage-test.poolSizeMB = 4096;
    boot.kernelModules = [ "nbd" ];
    virtualisation.memorySize = 2048;
    virtualisation.diskSize = 12 * 1024;
    networking.firewall.interfaces.exp0.allowedTCPPorts = [ 9440 ];
    environment.systemPackages = with pkgs; [ zfs nbd ];
  };
in
{
  name = "expanse-vol-create";

  nodes = {
    n1 = { ... }: nodeCommon 1;
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
  };

  testScript = ''
    ${builtins.readFile ./cluster-common.py}

    form("volcreate")

    with subtest("mesh is up — replicas talk over exp0"):
        for m in [n1, n2, n3]:
            out = m.succeed("ip -4 -o addr show exp0")
            assert "10.42." in out, f"{m.name}: no overlay address: {out}"

    with subtest("volume create completes in <= 10 s on the leader (G6.1, CLI half)"):
        import time
        start = time.time()
        out = n1.succeed("expanse ctl volume create testvol --size 10Gi")
        elapsed = time.time() - start
        assert elapsed <= 10, f"volume create took {elapsed:.1f}s (G6.1 budget 10s)"
        assert "create requested" in out, f"unexpected create output: {out}"

    with subtest("3 zvols exist, one per node (<= 90 s convergence)"):
        for m in [n1, n2, n3]:
            m.wait_until_succeeds(
                "zfs list -H -o name -t volume | grep -q '^volumes/volumes/vol-'", timeout=90
            )

    with subtest("device appears on exactly the primary; 1 GiB round-trips"):
        deadline = time.time() + 120
        holders = []
        while time.time() < deadline:
            holders = [m for m in [n1, n2, n3]
                       if m.execute("ls /dev/exvol")[1].strip() != ""]
            if len(holders) == 1:
                break
            time.sleep(2)
        assert len(holders) == 1, f"device on {[m.name for m in holders]}, want exactly 1"
        primary = holders[0]
        print(f"primary node: {primary.name}")
        primary.succeed("nbd-client -c /dev/nbd0")  # attached

        # 1 GiB of random data round-trips through the device. (No
        # globs: bash cannot expand `of=/dev/exvol/*` — the directory
        # part includes the assignment prefix — so resolve the path.)
        dev = "/dev/exvol/" + primary.succeed("ls -1 /dev/exvol").strip()
        primary.succeed("dd if=/dev/urandom of=/persist/pad bs=4M count=256")
        ref = primary.succeed("sha256sum /persist/pad | cut -d' ' -f1").strip()
        primary.succeed(f"dd if=/persist/pad of={dev} bs=4M count=256 conv=notrunc")
        got = primary.succeed(
            f"dd if={dev} bs=4M count=256 2>/dev/null | sha256sum | cut -d' ' -f1"
        ).strip()
        assert got == ref, f"sha256 mismatch: wrote {ref}, read back {got}"
  '';
}
