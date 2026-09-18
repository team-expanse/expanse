# §6 vol-durability (G6.3 — the release-blocker test). Runs the §4.8
# durability procedure: repeatedly write known-pattern records to the
# volume (fsync → ack), HARD-kill the volume-primary VM (qemu quit, not
# a clean shutdown), wait for failover, verify every acked record is
# present and byte-correct on the new primary, restore the killed node,
# wait for resync, and assert all 3 replicas' checksums agree.
#
# Any acked-write loss found here is a real bug in T04/T07/T11 — fix
# the protocol/recovery code; do not adjust the test.
#
# Iteration count: the flake check runs a compressed count (default 20)
# so `nix build` stays tractable; EXPANSE_DURABILITY_ITERS overrides at
# driver runtime (the driver runs outside the build sandbox when
# invoked via .driver, so CI's nightly job can do:
#   EXPANSE_DURABILITY_ITERS=500 \
#     $(nix build .#checks.x86_64-linux.vol-durability.driver --print-out-paths)/bin/nixos-test-driver
# for the full 500-iteration release run).
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
    # Stable raft advertise across reboots: a crash-rejoin must not
    # re-register the per-boot SLIRP address (identical on every VM).
    expanse.agent.raftAdvertise = "192.168.1.${toString idx}:7444";
    expanse.storage-test.poolSizeMB = 6144;
    boot.kernelModules = [ "nbd" ];
    virtualisation.memorySize = 2048;
    virtualisation.diskSize = 12 * 1024;
    networking.firewall.interfaces.exp0.allowedTCPPorts = [ 9440 ];
    environment.systemPackages = with pkgs; [ zfs nbd python3 ];
  };
  # py_compile runs at BUILD time — a syntax error fails the check
  # build, not a 6-minute VM run.
  recScript = pkgs.runCommand "vol-durability-rec.py" { } ''
    ${pkgs.python3}/bin/python3 -m py_compile ${./python/vol_durability_rec.py}
    ${pkgs.python3}/bin/python3 -m py_compile ${./python/vol_durability_main.py}
    cat ${./cluster-common.py} ${./python/vol_durability_main.py} | ${pkgs.python3}/bin/python3 -c 'import sys; compile(sys.stdin.read(), "testscript", "exec")'
    cp ${./python/vol_durability_rec.py} $out
  '';
in
{
  name = "expanse-vol-durability";

  nodes = {
    n1 = { ... }: nodeCommon 1;
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
  };

  testScript =   ''
    ${builtins.readFile ./cluster-common.py}
    REC = "/tmp/vol-durability-rec.py"
    for m in [n1, n2, n3]:
        m.copy_from_host("${recScript}", REC)
    ${builtins.readFile ./python/vol_durability_main.py}
  '';

}
