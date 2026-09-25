# Phase 11 X2, the release blocker: storage, cluster and network faults injected
# concurrently (not one fault family at a time, unlike every prior phase's own vertical
# slice) over an extended soak, with zero acked-write loss throughout. The scenario is
# python/chaos_soak_main.py; see its docstring for how the three fault pools are kept
# safe to run concurrently on a 3-node, quorum-2 cluster.
#
# The soak length defaults to an hour; EXPANSE_CHAOS_SOAK_SECONDS overrides it when the
# driver runs outside the build sandbox, mirroring vol-durability.nix's own override:
#   EXPANSE_CHAOS_SOAK_SECONDS=14400 \
#     $(nix build .#checks.x86_64-linux.chaos-soak.driver --print-out-paths)/bin/nixos-test-driver
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "chaos-soak-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    for f in ${./cluster-common.py} ${./python/vol_cluster.py} ${./python/chaos_soak_main.py}; do
      python3 -c 'import sys; compile(open(sys.argv[1]).read(), sys.argv[1], "exec")' $f
    done
    touch $out
  '';
  # The recorder runs on the VMs as a standalone executable (identical to vol-durability.nix's).
  recorder = pkgs.runCommand "chaos-soak-rec" { } ''
    { echo "#!${pkgs.python3}/bin/python3"; cat ${./python/vol_durability_rec.py}; } > $out
    chmod +x $out
  '';
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
    expanse.storage-test.enable = true;
    # A crash-restored node must rejoin raft at a stable address, not the per-boot one.
    expanse.agent.raftAdvertise = "192.168.1.${toString idx}:7444";
    # The acked-record ledger listens on the test network, apart from the mesh under test.
    networking.firewall.allowedTCPPorts = [ 9440 ];
    virtualisation.memorySize = 2048;
  };
in
{
  name = "expanse-chaos-soak";

  nodes = {
    n1 = { ... }: nodeCommon 1;
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./python/vol_cluster.py}
    REC = "/root/chaos-soak-rec"
    for m in [n1, n2, n3]:
        m.copy_from_host("${recorder}", REC)
    ${builtins.readFile ./python/chaos_soak_main.py}
  '';
}
