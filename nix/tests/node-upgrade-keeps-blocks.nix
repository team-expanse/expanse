# Phase 15 A3: restarting the agent, and switching to a new expanse build, leave a running block
# and the volume it holds open untouched. The scenario is python/upgrade_keeps_blocks.py.
{ self }:
{ pkgs, lib, ... }:
let
  shim = "n2 = n3 = n1\n";
  lint = pkgs.runCommand "node-upgrade-keeps-blocks-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    { cat ${./cluster-common.py} ${./block-common.py}; printf '%s' ${lib.escapeShellArg shim}; cat ${./python/vol_cluster.py} ${./python/single_node_common.py} ${./python/upgrade_keeps_blocks.py}; } \
      | python3 -c 'import sys; compile(sys.stdin.read(), "testscript", "exec")'
    touch $out
  '';
in
{
  name = "expanse-node-upgrade-keeps-blocks";

  nodes.n1 = { ... }: {
    imports = [
      self.nixosModules.expanse
      ../modules/storage-test.nix
    ];
    nixpkgs.overlays = [
      (final: prev: { expanse = self.packages.${prev.system}.expanse; })
    ];
    expanse.node.enable = true;
    expanse.agent.enable = true;
    expanse.hostId = "00000001";
    expanse.hostname = "n1";
    expanse.storage-test.enable = true;
    expanse.agent.raftAdvertise = "192.168.1.1:7444";
    expanse.agent.period = "5s";
    expanse.agent.controllerPeriod = "5s";
    expanse.agent.blocksCatalog = ../blocks;
    expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
    environment.etc."expanse/blocks-flake".source = ../blocks-flake;
    environment.systemPackages = with pkgs; [ curl ];
    virtualisation.memorySize = 2048;
    # The "new release": the same source under another version, so every unit naming it changes.
    specialisation.upgraded.configuration.nixpkgs.overlays = [
      (final: prev: { expanse = prev.expanse.override { version = "99.0.0-upgraded"; }; })
    ];
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./block-common.py}
    ${shim}
    ${builtins.readFile ./python/vol_cluster.py}
    ${builtins.readFile ./python/single_node_common.py}
    ${builtins.readFile ./python/upgrade_keeps_blocks.py}
  '';
}
