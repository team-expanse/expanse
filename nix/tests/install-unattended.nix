# Unattended install: a live installer-like system (2 cores, 2 GB RAM,
# blank 20 GB disk) runs `expanse install` through the real stage
# pipeline — disko partitioning with our layout file, blank snapshot,
# identity, config generation, verification — minus the final
# `nixos-install` evaluation, which requires the flake source and full
# system closure in the store. That final stage is exercised by the
# two-VM ISO boot harness in CI (see docs/DEVELOPING.md).
{ self }:
{ pkgs, lib, ... }:
let
  expanse = self.packages.${pkgs.system}.expanse;
  installConfig = pkgs.writeText "expanse-install.yaml" ''
    version: 1
    disks:
      layout: single
      devices: [/dev/vdb]
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
  name = "expanse-install-unattended";

  nodes.machine = { config, pkgs, lib, ... }: {
    environment.systemPackages = with pkgs; [ expanse disko btrfs-progs lvm2 e2fsprogs util-linux ];
    networking.hostId = "01234567";

    # The flake source, as the ISO bakes it (see nix/installer/iso.nix).
    environment.etc."expanse/flake".source = self;
    # disko's evaluator needs <nixpkgs>; the ISO has a channel, the test
    # VM points at the store path it was evaluated from.
    environment.variables.NIX_PATH = lib.mkForce "nixpkgs=${toString pkgs.path}";

    virtualisation.memorySize = 2048;
    virtualisation.cores = 2;
    virtualisation.emptyDiskImages = [ 20480 ];

    # disko evaluates and builds its runtime script inside the (offline)
    # VM; pre-build the exact same derivation here so it is already in
    # the VM's store. This mirrors what the ISO gets for free from its
    # baked-in channel + substitutes.
    virtualisation.additionalPaths = [
      ((import "${pkgs.disko}/share/disko" { inherit lib; })._cliDestroyFormatMount
        (import (self + "/nix/installer/disko/single.nix") { disks = [ "/dev/vdb" ]; })
        (import pkgs.path { system = pkgs.stdenv.hostPlatform.system; }))
    ];
  };

  testScript = ''
    machine.start()
    machine.wait_for_unit("multi-user.target")
    machine.succeed("cp ${installConfig} /tmp/install.yaml")

    with subtest("dry-run prints every stage and touches nothing"):
        out = machine.succeed("expanse install --config /tmp/install.yaml --dry-run 2>&1")
        for stage in ("preflight", "detect", "confirm", "partition", "snapshot", "config", "install", "identity", "verify"):
            assert stage in out, f"dry-run missing stage {stage}"
        assert "btrfs subvolume snapshot" in out

    with subtest("unattended install completes under 600s"):
        machine.succeed(
            "NIX_PATH=nixpkgs=${pkgs.path} time expanse install --config /tmp/install.yaml --force --skip-system-install --target-flake /etc/expanse/flake >&2",
            timeout=600,
        )

    with subtest("subvolumes exist and the data VG is there"):
        out = machine.succeed("btrfs subvolume list /mnt")
        for sv in ["@root", "@nix", "@persist", "@log"]:
            assert sv in out, f"missing subvolume {sv}: {out}"
        machine.succeed("vgs expanse")

    with subtest("blank snapshot exists"):
        out = machine.succeed("btrfs subvolume list /mnt")
        assert "@root-blank" in out, out

    with subtest("identity exists on the target and is a valid UUID"):
        nid = machine.succeed("cat /mnt/persist/expanse/identity/node-id").strip()
        assert len(nid) == 36, f"node-id not a UUID: {nid}"
        machine.succeed("test $(stat -c %a /mnt/persist/expanse/identity/node.key) = 600")

    with subtest("install config persisted"):
        machine.succeed("test -s /mnt/persist/expanse/install-config.yaml")

    with subtest("generated configuration.nix imports the module set"):
        conf = machine.succeed("cat /mnt/persist/etc/nixos/configuration.nix")
        assert "expanse-node.nix" in conf, conf
        assert "expanse.hostId" in conf, conf

    with subtest("footprint within 6G budget"):
        usage = machine.succeed("btrfs filesystem usage -b /mnt")
        used = None
        for line in usage.splitlines():
            line = line.strip()
            if line.startswith("Used:"):
                used = int(line.split()[1])
                break
        assert used is not None, f"no Used: line in btrfs filesystem usage: {usage}"
        assert used < 6 * 1024**3, f"system partition used {used} bytes > 6G"

    with subtest("second run is re-runnable (fresh identity by design)"):
        # A --force install destroys the pool, so identity is regenerated;
        # identity stability across boots is covered by the impermanence
        # test. What matters here: the installer can run again cleanly.
        machine.succeed(
            "NIX_PATH=nixpkgs=${pkgs.path} expanse install --config /tmp/install.yaml --force --skip-system-install --target-flake /etc/expanse/flake >&2",
            timeout=600,
        )
        nid2 = machine.succeed("cat /mnt/persist/expanse/identity/node-id").strip()
        assert len(nid2) == 36, f"node-id not a UUID: {nid2}"
        out = machine.succeed("btrfs subvolume list /mnt")
        assert "@root-blank" in out, out
  '';
}
