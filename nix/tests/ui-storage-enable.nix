# Phase 14 E: storage turned on from a node's page in the web UI, on a one-node cluster whose Nix
# config sets no storageVG. The scenario is python/ui_storage_enable.py.
{ self }:
{ pkgs, lib, ... }:
let
  # vol_cluster.py's NODES names n1..n3; this test has n1 only (NODES is unused here).
  shim = "n2 = n3 = n1\n";
  lint = pkgs.runCommand "ui-storage-enable-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    { cat ${./cluster-common.py} ${./block-common.py}; printf '%s' ${lib.escapeShellArg shim}; cat ${./python/vol_cluster.py} ${./python/single_node_common.py} ${./python/ui_forms_common.py} ${./python/ui_storage_enable.py}; } \
      | python3 -c 'import sys; compile(sys.stdin.read(), "testscript", "exec")'
    touch $out
  '';
in
{
  name = "expanse-ui-storage-enable";

  nodes.n1 = { ... }: {
    imports = [ self.nixosModules.expanse ./curl-cookie-jar.nix ];
    nixpkgs.overlays = [
      (final: prev: { expanse = self.packages.${prev.system}.expanse; })
    ];
    expanse.node.enable = true;
    expanse.agent.enable = true;
    expanse.hostId = "00000001";
    expanse.hostname = "n1";
    expanse.agent.raftAdvertise = "192.168.1.1:7444";
    expanse.agent.period = "5s";
    expanse.agent.controllerPeriod = "5s";
    expanse.agent.blocksCatalog = ../blocks;
    expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
    environment.etc."expanse/blocks-flake".source = ../blocks-flake;
    # The installer's data VG on its own disk, with nothing built on it; no storageVG.
    virtualisation.emptyDiskImages = [ 1024 ];
    systemd.services.data-vg = {
      wantedBy = [ "multi-user.target" ];
      path = [ pkgs.lvm2 ];
      serviceConfig.Type = "oneshot";
      serviceConfig.RemainAfterExit = true;
      script = "vgs expanse || { pvcreate /dev/vdb && vgcreate expanse /dev/vdb; }";
    };
    environment.systemPackages = with pkgs; [ curl ];
    virtualisation.memorySize = 2048;
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./block-common.py}
    ${shim}
    ${builtins.readFile ./python/vol_cluster.py}
    ${builtins.readFile ./python/single_node_common.py}
    ${builtins.readFile ./python/ui_forms_common.py}
    ${builtins.readFile ./python/ui_storage_enable.py}
  '';
}
