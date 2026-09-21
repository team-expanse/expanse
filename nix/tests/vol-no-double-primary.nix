# Phase 1 E6 (X5): through a seeded partition storm no two nodes are ever DRBD Primary at once.
# The scenario is python/vol_no_double_primary_main.py; its role analysis is python/vol_roles.py.
{ self }:
{ pkgs, lib, ... }:
let
  # Unit tests and syntax checks run here, at build time, before any VM boots.
  lint = pkgs.runCommand "vol-no-double-primary-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    for f in ${./cluster-common.py} ${./python/vol_cluster.py} ${./python/vol_no_double_primary_main.py}; do
      python3 -c 'import sys; compile(open(sys.argv[1]).read(), sys.argv[1], "exec")' $f
    done
    mkdir tests
    cp ${./python/vol_roles.py} tests/vol_roles.py
    cp ${./python/vol_roles_test.py} tests/vol_roles_test.py
    (cd tests && python3 -m unittest vol_roles_test)
    touch $out
  '';
  # The scenario imports the analysis rather than embedding it, so the unit tests run the same file.
  rolesDir = pkgs.runCommand "vol-roles" { } ''
    mkdir $out
    cp ${./python/vol_roles.py} $out/vol_roles.py
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
  name = "expanse-vol-no-double-primary";

  nodes = {
    n1 = { ... }: nodeCommon 1;
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
  };

  testScript = ''
    # ${lint}
    import importlib
    import sys

    sys.path.insert(0, "${rolesDir}")
    roles = importlib.import_module("vol_roles")

    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./python/vol_cluster.py}
    ${builtins.readFile ./python/vol_no_double_primary_main.py}
  '';
}
