# PHASE-04-TASKS.md Stream C (X2, the phase's decider; X4): a SINGLETON
# iscsi/target block survives a hard kill of the node serving it -- the
# VIP moves, DRBD promotes on the survivor, and the initiator's own
# open-iscsi session recovery resumes writes without a manual re-login
# (the same "slow path" share-smb-failover.nix already proved for
# SINGLETON, PHASE-03-TASKS.md Stream B2). A second sub-test measures
# whether a SCSI-3 persistent reservation survives the same failover
# (X4, not a release gate -- see D4).
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "iscsi-target-failover-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    for f in ${./cluster-common.py} ${./block-common.py} ${./client-common.py} ${./python/vol_cluster.py} ${./python/iscsi_target_failover.py}; do
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
  name = "expanse-iscsi-target-failover";

  nodes = {
    n1 = { ... }: nodeCommon 1;
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
    # The external initiator: a plain machine on the same LAN, no
    # expanse. Named "n9" so the driver's name-sorted eth1 assignment
    # leaves 192.168.1.1-.3 for the cluster nodes (iscsi-target.nix
    # convention). sg3_utils: sg_persist, the standard initiator-side
    # tool for issuing SCSI-3 PERSISTENT RESERVE commands (X4) -- a test
    # dependency only, not a production one (D1's revision dropped
    # multipath-tools/sg3_utils from the production posture; this is the
    # unrelated, initiator-side PR tool, not multipath tooling).
    n9 = { ... }: {
      virtualisation.memorySize = 1024;
      networking.firewall.enable = false;
      services.openiscsi = {
        enable = true;
        name = "iqn.2020-08.org.linux-iscsi.initiator:failover";
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
    VIP_POOL = ${builtins.toJSON vipPool}
    ${builtins.readFile ./python/iscsi_target_failover.py}
  '';
}
