# PHASE-08-TASKS.md Stream C (X4, X5): the generations store's own
# desired-state history, and cluster identity material already living
# under dataDir (D2), both backed up and restored via restic against a
# real S3-compatible target, provably restorable -- a real reconcile
# after re-importing the generation snapshot, and a real rejoin under
# the restored identity, not just "the restore command exited 0".
# The scenario is python/backup_cluster_config_main.py.
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "backup-cluster-config-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    for f in ${./cluster-common.py} ${./python/backup_cluster_config_main.py}; do
      python3 -c 'import sys; compile(open(sys.argv[1]).read(), sys.argv[1], "exec")' $f
    done
    touch $out
  '';
  nodeCommon = idx: {
    imports = [ self.nixosModules.expanse ];
    nixpkgs.overlays = [
      (final: prev: { expanse = self.packages.${prev.system}.expanse; })
    ];
    expanse.node.enable = true;
    expanse.agent.enable = true;
    expanse.hostId = "0000000${toString idx}";
    expanse.hostname = "n${toString idx}";
    virtualisation.memorySize = 1536;
  };
in
{
  name = "expanse-backup-cluster-config";

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
    ${builtins.readFile ./python/backup_cluster_config_main.py}
  '';
}
