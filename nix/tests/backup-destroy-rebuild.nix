# PHASE-08-TASKS.md Stream D (X6, the decider): the vertical slice.
# Every node's cluster state -- identity, raft log, generations/config --
# is destroyed simultaneously (no survivor, no live quorum to catch up
# from), then rebuilt from backup credentials with the one new
# `expanse cluster restore` command, run once per node. Data (a real
# replicated volume, zeroed to simulate loss) and configuration (a real
# desired-state resource) are both independently verified correct
# afterward against their pre-destruction reference values, reusing the
# exact restic mechanisms Streams A-C already proved -- integration, not
# new mechanism, per this phase's own framing.
# The scenario is python/backup_destroy_rebuild_main.py.
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "backup-destroy-rebuild-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    for f in ${./cluster-common.py} ${./python/vol_cluster.py} ${./python/backup_destroy_rebuild_main.py}; do
      python3 -c 'import sys; compile(open(sys.argv[1]).read(), sys.argv[1], "exec")' $f
    done
    touch $out
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
    expanse.agent.raftAdvertise = "192.168.1.${toString idx}:7444";
    environment.systemPackages = [ pkgs.restic ];
    virtualisation.memorySize = 2048;
  };
in
{
  name = "expanse-backup-destroy-rebuild";

  nodes = {
    # garage lives on n1 but outside /persist/expanse, so it is untouched
    # by the destroy step below -- the same off-cluster durability a real
    # external S3-compatible backup target has in production.
    n1 = { pkgs, ... }: {
      imports = [ (nodeCommon 1) ];
      services.garage = {
        enable = true;
        package = pkgs.garage;
        settings = {
          replication_factor = 1;
          rpc_bind_addr = "[::]:3901";
          rpc_public_addr = "127.0.0.1:3901";
          # Test-only secret, fixed so the test is reproducible; never used
          # outside this disposable VM.
          rpc_secret = "b0aa753b23ae60c8c7baf46d0fb34cdf43806570f977dd9d81473072c72de5f0";
          s3_api = {
            s3_region = "garage";
            # Bound cluster-wide (not just loopback): n2/n3 back up to and
            # restore from it too, unlike every earlier Phase 8 test where
            # only the single node hosting garage ever spoke to it.
            api_bind_addr = "0.0.0.0:3900";
            root_domain = ".s3.garage.localhost";
          };
        };
      };
      networking.firewall.allowedTCPPorts = [ 3900 ];
    };
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./python/vol_cluster.py}
    ${builtins.readFile ./python/backup_destroy_rebuild_main.py}
  '';
}
