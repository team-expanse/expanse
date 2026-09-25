# PHASE-08-TASKS.md Stream B (X3): btrfs-backed durable node/cluster state
# under /persist (D2: node identity, ssh host key, and the impermanence
# bind-mount sources) backed up and restored via restic against a real
# S3-compatible target, the same rigor X1 applied to a single file -- dedupe,
# encryption and restore-integrity all exercised for real, against this
# node's actual persisted content, not a synthetic stand-in.
# The scenario is python/backup_persist_main.py.
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "backup-persist-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    python3 -c 'import sys; compile(open(sys.argv[1]).read(), sys.argv[1], "exec")' ${./python/backup_persist_main.py}
    touch $out
  '';
in
{
  name = "expanse-backup-persist";

  nodes.machine = { config, pkgs, ... }: {
    imports = [ self.nixosModules.expanse ./pool-init.nix ];
    nixpkgs.overlays = [
      (final: prev: { expanse = self.packages.${prev.system}.expanse; })
    ];
    expanse.node.enable = true;
    expanse.hostId = "01234567";
    expanse.hostname = "expanse-persist-test";

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

    virtualisation.memorySize = 2048;
    virtualisation.cores = 2;
    virtualisation.emptyDiskImages = [ 4096 ];
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./python/backup_persist_main.py}
  '';
}
