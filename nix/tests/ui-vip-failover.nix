# Phase 2, A3: the web UI gets its own VIP (D8), independent of block
# VIPs. Kill the node holding it: a survivor announces the same address
# (vip_failover_ms budget) and a session cookie obtained before the kill
# still authorizes a request after it (X7 -- sessions live in the Raft
# store, not process memory).
{ self }:
{ pkgs, lib, ... }:
let
  nodeCommon = idx: {
    imports = [ self.nixosModules.expanse ];
    nixpkgs.overlays = [
      (final: prev: { expanse = self.packages.${prev.system}.expanse; })
    ];
    expanse.node.enable = true;
    expanse.agent.enable = true;
    expanse.hostId = "0000000${toString idx}";
    expanse.hostname = "n${toString idx}";
    virtualisation.memorySize = 1536;
    # §4.2 external pool: a single address keeps the test deterministic.
    # Distinct from the block-VIP tests' 192.168.1.100 to avoid any
    # cross-test confusion if ever run against the same LAN segment.
    expanse.agent.externalVIPPool = "192.168.1.150-192.168.1.150";
    expanse.agent.externalInterface = "eth1";
    networking.firewall.allowedTCPPorts = [ 8443 7443 7444 7445 7446 ];
  };
  lint = pkgs.runCommand "ui-vip-failover-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    cd ${./python}
    cat ${./cluster-common.py} ui_vip_failover.py \
      | python3 -c 'import sys; compile(sys.stdin.read(), "testscript", "exec")'
    touch $out
  '';
in
{
  name = "expanse-ui-vip-failover";

  # Every node also gets curl-cookie-jar.nix (curl/curl#23261).
  nodes = lib.mapAttrs (_: node: { imports = [ node ./curl-cookie-jar.nix ]; }) {
    n1 = { ... }: nodeCommon 1;
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
    # The external client: a plain machine on the same LAN, no expanse.
    # Named n9 so the driver's name-sorted eth1 assignment leaves
    # 192.168.1.1-.3 for the cluster nodes (cluster-common.py hardcodes
    # n1=192.168.1.1).
    n9 = { ... }: {
      virtualisation.memorySize = 1024;
      networking.firewall.enable = false;
      environment.systemPackages = [ pkgs.curl ];
    };
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./python/ui_vip_failover.py}
  '';
}
