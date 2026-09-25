# PHASE-08-TASKS.md Stream B (X2): opaque block-volume data (LVM thin
# snapshots, storage.Class.Snapshot/RestoreSnapshot, Phase 1) backed up and
# restored via restic against a real S3-compatible target (garage, same
# fixture X1's backup-basic.nix uses), content verified checksum-equal. Also
# closes R1/D5's concurrent-write concern for real: the snapshot is backed up
# while the live volume keeps being overwritten, and the restore must come
# back as the pre-snapshot bytes, never a torn mix.
# The scenario is python/backup_volume_snapshot_main.py.
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "backup-volume-snapshot-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    for f in ${./cluster-common.py} ${./python/vol_cluster.py} ${./python/backup_volume_snapshot_main.py}; do
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
    virtualisation.memorySize = 2048;
  };
in
{
  name = "expanse-backup-volume-snapshot";

  nodes = {
    n1 = { pkgs, ... }: {
      imports = [ (nodeCommon 1) ];
      environment.systemPackages = [ pkgs.restic ];
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
            api_bind_addr = "127.0.0.1:3900";
            root_domain = ".s3.garage.localhost";
          };
        };
      };
    };
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./python/vol_cluster.py}
    ${builtins.readFile ./python/backup_volume_snapshot_main.py}
  '';
}
