# PHASE-04-TASKS.md Stream D (D1): the phase-closing vertical slice --
# X1, X2, X4 and X5 all proven in one combined scenario, the way
# ui-vertical-slice.nix closed Phase 2 and Phase 3's (paused) D1 would
# have closed Phase 3 per-protocol. Unlike Stream C's
# iscsi-target-failover.nix, the kill lands the instant the write loop
# is confirmed flowing, with no internal placement/DRBD/VIP-settle
# checks assumed beforehand -- "kill mid-deploy, not after".
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "iscsi-vertical-slice-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    for f in ${./cluster-common.py} ${./block-common.py} ${./client-common.py} ${./python/vol_cluster.py} ${./python/iscsi_common.py} ${./python/iscsi_vertical_slice.py}; do
      python3 -c 'import sys; compile(open(sys.argv[1]).read(), sys.argv[1], "exec")' $f
    done
    touch $out
  '';
  # Two addresses: the cluster's own management UI is a permanent,
  # always-on consumer of the SAME external pool (iscsi-target.nix's own
  # note) -- a size-1 pool leaves nothing for the target's own VIP.
  vipPool = [ "192.168.1.100" "192.168.1.101" ];
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
    expanse.agent.period = "5s";
    expanse.agent.controllerPeriod = "5s";
    expanse.agent.blocksCatalog = ../blocks;
    expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
    environment.etc."expanse/blocks-flake".source = ../blocks-flake;
    expanse.agent.externalVIPPool = "${builtins.elemAt vipPool 0}-${builtins.elemAt vipPool 1}";
    expanse.agent.externalInterface = "eth1";
    networking.firewall.allowedTCPPorts = [ 3260 ];
    boot.kernelModules = [ "configfs" "target_core_mod" "iscsi_target_mod" ];
    environment.systemPackages = [ pkgs.curl pkgs.jq pkgs.targetcli-fb ];
    virtualisation.memorySize = 1536;
  };
in
{
  name = "expanse-iscsi-vertical-slice";

  nodes = {
    n1 = { ... }: nodeCommon 1;
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
    # The external initiator (iscsi-target-failover.nix convention: n9,
    # so the driver's name-sorted eth1 assignment leaves 192.168.1.1-.3
    # for the cluster nodes). sg3_utils: sg_persist, test-only (D1's
    # revision dropped multipath tooling, not PR tooling).
    n9 = { ... }: {
      virtualisation.memorySize = 1024;
      networking.firewall.enable = false;
      services.openiscsi = {
        enable = true;
        name = "iqn.2020-08.org.linux-iscsi.initiator:vslice";
      };
      environment.systemPackages = [ pkgs.curl pkgs.sg3_utils ];
    };
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./client-common.py}
    client = n9  # the external initiator VM
    ${builtins.readFile ./block-common.py}
    ${builtins.readFile ./python/vol_cluster.py}
    ${builtins.readFile ./python/iscsi_common.py}
    VIP_POOL = ${builtins.toJSON vipPool}
    ${builtins.readFile ./python/iscsi_vertical_slice.py}
  '';
}
