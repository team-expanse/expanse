# PHASE-03-TASKS.md Stream B2 (X2, the phase's decider for SMB): a client
# writes continuously through a share/smb block's CIFS mount, the serving
# node is hard-killed mid-write, and the block (process, volume primary,
# VIP) reschedules to a survivor -- the client's own reconnect resumes
# writes without a manual remount, and a second, independent reader (the
# host-level volume mount) agrees byte for byte with the client's own
# checksum. Combines share-smb.nix's substrate (Stream B1) with
# share-colocation.nix's kill/reconverge pattern (Stream A1).
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "share-smb-failover-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    for f in ${./cluster-common.py} ${./block-common.py} ${./client-common.py} ${./python/vol_cluster.py} ${./python/share_smb_failover.py}; do
      python3 -c 'import sys; compile(open(sys.argv[1]).read(), sys.argv[1], "exec")' $f
    done
    touch $out
  '';
  # Two addresses: the cluster's own management UI is a permanent,
  # always-on consumer of the SAME external pool (share-smb.nix's note,
  # internal/agent/ui_vip.go) -- a size-1 pool leaves nothing for the
  # share's own VIP once the UI has claimed the only address.
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
    # Tight reconcile period: both the block controller's reschedule and
    # the storage controller's movePrimaryForBlock need to converge
    # within the test's failover budget.
    expanse.agent.period = "5s";
    expanse.agent.controllerPeriod = "5s";
    expanse.agent.blocksCatalog = ../blocks;
    expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
    environment.etc."expanse/blocks-flake".source = ../blocks-flake;
    expanse.agent.externalVIPPool = "${builtins.elemAt vipPool 0}-${builtins.elemAt vipPool 1}";
    expanse.agent.externalInterface = "eth1";
    networking.firewall.allowedTCPPorts = [ 445 ];
    environment.systemPackages = [ pkgs.curl pkgs.jq pkgs.samba ];
    virtualisation.memorySize = 1536;
  };
in
{
  name = "expanse-share-smb-failover";

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
    ${builtins.readFile ./python/vol_cluster.py}
    VIP_POOL = ${builtins.toJSON vipPool}
    ${builtins.readFile ./python/share_smb_failover.py}
  '';
}
