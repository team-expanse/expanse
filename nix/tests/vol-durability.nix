# Phase 1 X1, the release gate: no acked write is lost across hard crashes of the volume's
# primary, and every replica ends byte-identical. The scenario is python/vol_durability_main.py.
#
# The iteration count defaults to 20; EXPANSE_DURABILITY_ITERS overrides it when the driver runs
# outside the build sandbox:
#   EXPANSE_DURABILITY_ITERS=100 \
#     $(nix build .#checks.x86_64-linux.vol-durability.driver --print-out-paths)/bin/nixos-test-driver
{ self }:
{ pkgs, lib, ... }:
let
  # Unit tests and syntax checks run here, at build time, before any VM boots.
  lint = pkgs.runCommand "vol-durability-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    for f in ${./cluster-common.py} ${./python/vol_cluster.py} ${./python/vol_durability_main.py}; do
      python3 -c 'import sys; compile(open(sys.argv[1]).read(), sys.argv[1], "exec")' $f
    done
    mkdir tests
    cp ${./python/vol_durability_rec.py} tests/vol_durability_rec.py
    cp ${./python/vol_durability_rec_test.py} tests/vol_durability_rec_test.py
    (cd tests && python3 -m unittest vol_durability_rec_test)
    touch $out
  '';
  # The recorder runs on the VMs as a standalone executable.
  recorder = pkgs.runCommand "vol-durability-rec" { } ''
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
  name = "expanse-vol-durability";

  nodes = {
    n1 = { ... }: nodeCommon 1;
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./python/vol_cluster.py}
    REC = "/root/vol-durability-rec"
    for m in [n1, n2, n3]:
        m.copy_from_host("${recorder}", REC)
    ${builtins.readFile ./python/vol_durability_main.py}
  '';
}
