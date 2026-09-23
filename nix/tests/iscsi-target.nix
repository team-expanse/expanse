# PHASE-04-TASKS.md Stream B: deploy a SINGLETON iscsi/target block on a
# raw (no-filesystem) DRBD-backed volume; an external open-iscsi initiator
# discovers, logs in through the block's VIP, and does I/O (X1). Combines
# storage-test.nix (DRBD/LVM, Phase 1) with blocksCatalog (Phase 04) and
# an external VIP pool (Phase 05), the same substrate share-smb.nix (D1's
# VIP/SINGLETON fallback, PHASE-03-TASKS.md B1) already proved for a
# filesystem-backed share — here proved for a raw block LUN instead.
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "iscsi-target-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    for f in ${./cluster-common.py} ${./block-common.py} ${./client-common.py} ${./python/vol_cluster.py} ${./python/iscsi_target.py}; do
      python3 -c 'import sys; compile(open(sys.argv[1]).read(), sys.argv[1], "exec")' $f
    done
    touch $out
  '';
  # Two addresses: the cluster's own management UI is a permanent,
  # always-on consumer of the SAME external pool (internal/agent/ui_vip.go
  # allocates on every node.enable=true agent, independent of any block) —
  # a size-1 pool leaves nothing for the target's own VIP once the UI has
  # claimed the only address. iscsi_target.py discovers which of the two
  # the block actually got rather than assuming an address.
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
    # §4.2 external pool: two addresses (see vipPool's comment above).
    expanse.agent.externalVIPPool = "${builtins.elemAt vipPool 0}-${builtins.elemAt vipPool 1}";
    expanse.agent.externalInterface = "eth1";
    networking.firewall.allowedTCPPorts = [ 3260 ];
    # LIO's kernel pieces, and targetcli-fb to drive them — a real
    # upstream toolchain exec'd from PATH (block-catalog.nix convention),
    # the deployment image ships it. drbdadm/drbdsetup already land on
    # PATH via services.drbd (storage-test.nix), so nothing extra is
    # needed there.
    boot.kernelModules = [ "configfs" "target_core_mod" "iscsi_target_mod" ];
    environment.systemPackages = [ pkgs.curl pkgs.jq pkgs.targetcli-fb ];
    virtualisation.memorySize = 1536;
  };
in
{
  name = "expanse-iscsi-target";

  nodes = {
    n1 = { ... }: nodeCommon 1;
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
    # The external initiator: a plain machine on the same LAN, no
    # expanse. Named "n9" so the driver's name-sorted eth1 assignment
    # leaves 192.168.1.1-.3 for the cluster nodes (net-vip-basic.nix,
    # share-smb.nix convention).
    n9 = { ... }: {
      virtualisation.memorySize = 1024;
      networking.firewall.enable = false;
      services.openiscsi = {
        enable = true;
        name = "iqn.2020-08.org.linux-iscsi.initiator:test";
      };
      environment.systemPackages = [ pkgs.curl ];
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
    ${builtins.readFile ./python/iscsi_target.py}
  '';
}
