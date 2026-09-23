# PHASE-03-TASKS.md Stream B1: deploy a SINGLETON share/smb block on a
# DRBD-backed volume; an external client mounts it over CIFS through the
# block's VIP and writes a file whose content and checksum verify from a
# second, independent read (X1). Combines storage-test.nix (DRBD/LVM,
# Phase 1) with blocksCatalog (Phase 04) and an external VIP pool
# (Phase 05), the same substrate share-colocation.nix (Stream A1) and
# net-vip-basic.nix (Phase 05) each proved independently.
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "share-smb-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    for f in ${./cluster-common.py} ${./block-common.py} ${./client-common.py} ${./python/share_smb.py}; do
      python3 -c 'import sys; compile(open(sys.argv[1]).read(), sys.argv[1], "exec")' $f
    done
    touch $out
  '';
  # Two addresses: the cluster's own management UI is a permanent,
  # always-on consumer of the SAME external pool (internal/agent/ui_vip.go
  # allocates on every node.enable=true agent, independent of any block) —
  # a size-1 pool leaves nothing for the share's own VIP once the UI has
  # claimed the only address. share_smb.py discovers which of the two the
  # block actually got rather than assuming an address.
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
    networking.firewall.allowedTCPPorts = [ 445 ];
    # smbd is a real upstream binary, exec'd from PATH (block-catalog.nix
    # convention) — the deployment image ships it.
    environment.systemPackages = [ pkgs.curl pkgs.jq pkgs.samba ];
    virtualisation.memorySize = 1536;
  };
in
{
  name = "expanse-share-smb";

  nodes = {
    n1 = { ... }: nodeCommon 1;
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
    # The external client: a plain machine on the same LAN, no expanse.
    # Named "n9" so the driver's name-sorted eth1 assignment leaves
    # 192.168.1.1-.3 for the cluster nodes (net-vip-basic.nix convention).
    n9 = { ... }: {
      virtualisation.memorySize = 1024;
      networking.firewall.enable = false;
      environment.systemPackages = with pkgs; [ curl cifs-utils ];
    };
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./client-common.py}
    client = n9  # the external client VM
    ${builtins.readFile ./block-common.py}
    VIP_POOL = ${builtins.toJSON vipPool}
    ${builtins.readFile ./python/share_smb.py}
  '';
}
