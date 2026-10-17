# 1.10.1: an installed node turns storage on with nixos-rebuild switch (docs/STORAGE.md §2), no reboot.
# The thin pool must be creatable at once, and volumes must refuse the in-tree DRBD 8.4 that the switch
# loads, with an error that says to reboot. The scenario is python/storage_enable_switch.py.
{ self }:
{ pkgs, lib, ... }:
let
  shim = "n2 = n3 = n1\n";
  lint = pkgs.runCommand "storage-enable-switch-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    { cat ${./cluster-common.py} ${./block-common.py}; printf '%s' ${lib.escapeShellArg shim}; cat ${./python/vol_cluster.py} ${./python/single_node_common.py} ${./python/storage_enable_switch.py}; } \
      | python3 -c 'import sys; compile(sys.stdin.read(), "testscript", "exec")'
    touch $out
  '';
in
{
  name = "expanse-storage-enable-switch";

  nodes.n1 = { ... }: {
    imports = [ self.nixosModules.expanse ];
    nixpkgs.overlays = [
      (final: prev: { expanse = self.packages.${prev.system}.expanse; })
    ];
    expanse.node.enable = true;
    expanse.agent.enable = true;
    expanse.hostId = "00000001";
    expanse.hostname = "n1";
    expanse.agent.period = "5s";
    expanse.agent.controllerPeriod = "5s";
    # The installer's data VG on its own disk, with nothing built on it.
    virtualisation.emptyDiskImages = [ 1024 ];
    systemd.services.data-vg = {
      wantedBy = [ "multi-user.target" ];
      path = [ pkgs.lvm2 ];
      serviceConfig.Type = "oneshot";
      serviceConfig.RemainAfterExit = true;
      script = "vgs expanse || { pvcreate /dev/vdb && vgcreate expanse /dev/vdb; }";
    };
    # What the operator adds to configuration.nix before switching.
    specialisation.storage.configuration = {
      expanse.agent.storageVG = "expanse";
      expanse.agent.storagePool = "pool";
    };
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./block-common.py}
    ${shim}
    ${builtins.readFile ./python/vol_cluster.py}
    ${builtins.readFile ./python/single_node_common.py}
    ${builtins.readFile ./python/storage_enable_switch.py}
  '';
}
