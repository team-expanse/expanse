# Phase 12 B3: a volume made on a one-node cluster grows 1 -> 2 -> 3 as nodes join, under
# continuous acked writes, with zero loss. The scenario is python/single_node_grow_main.py.
{ self }:
{ pkgs, lib, ... }:
let
  scripts = [ ./cluster-common.py ./python/vol_cluster.py ./python/single_node_common.py ./python/single_node_grow_main.py ];
  lint = pkgs.runCommand "cluster-single-node-grow-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    cat ${lib.concatMapStringsSep " " (p: "${p}") scripts} | python3 -c 'import sys; compile(sys.stdin.read(), "testscript", "exec")'
    touch $out
  '';
  # The vol-durability recorder, run on the VMs as a standalone executable.
  recorder = pkgs.runCommand "cluster-single-node-grow-rec" { } ''
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
    expanse.agent.raftAdvertise = "192.168.1.${toString idx}:7444";
    expanse.agent.period = "5s";
    expanse.agent.controllerPeriod = "5s";
    networking.firewall.allowedTCPPorts = [ 9440 ];
    virtualisation.memorySize = 1536;
  };
in
{
  name = "expanse-cluster-single-node-grow";

  nodes = {
    n1 = { ... }: nodeCommon 1;
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
  };

  testScript = ''
    # ${lint}
    RECORDER = "${recorder}"
    REC = "/root/grow-rec"
  '' + lib.concatMapStrings builtins.readFile scripts;
}
