# Phase 2, E1: the phase-closing vertical slice -- deploy nginx from
# the UI through its own VIP (D8), hard-kill the node holding that VIP
# mid-deploy, and confirm the interface survives and the deployment
# still completes (X2, the release blocker for this phase, the way
# E2's durability loop was for Phase 1). Combines ui-vip-failover.nix's
# external-client/VIP-kill pattern with ui-blocks.nix's deploy path.
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "ui-vertical-slice-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    cd ${./python}
    cat ${./cluster-common.py} ${./block-common.py} ui_vertical_slice.py \
      | python3 -c 'import sys; compile(sys.stdin.read(), "testscript", "exec")'
    touch $out
  '';
  nodeCommon = idx: {
    imports = [ self.nixosModules.expanse ];
    nixpkgs.overlays = [
      (final: prev: { expanse = self.packages.${prev.system}.expanse; })
    ];
    # nginx: the blocks-flake's web/nginx stub execs the bare "nginx"
    # command expecting it on PATH (net-vip-basic.nix's own nginx
    # deploy relies on the identical workaround).
    environment.systemPackages = with pkgs; [ curl jq nginx ];
    expanse.node.enable = true;
    expanse.agent.enable = true;
    expanse.hostId = "0000000${toString idx}";
    expanse.hostname = "n${toString idx}";
    virtualisation.memorySize = 2048;
    # Tight reconcile period (matches ui-blocks.nix): the deploy must
    # reach RUNNING within this test's polling budget, not on luck.
    expanse.agent.period = "5s";
    expanse.agent.controllerPeriod = "5s";
    expanse.agent.blocksCatalog = ../blocks;
    expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
    environment.etc."expanse/blocks-flake".source = ../blocks-flake;
    # §4.2 external pool: a single address keeps the test deterministic
    # (D8's own UI VIP, distinct from any block-exposed VIP address).
    expanse.agent.externalVIPPool = "192.168.1.150-192.168.1.150";
    expanse.agent.externalInterface = "eth1";
    networking.firewall.allowedTCPPorts = [ 8443 7443 7444 7445 7446 ];
  };
in
{
  name = "expanse-ui-vertical-slice";

  # Every node also gets curl-cookie-jar.nix (curl/curl#23261).
  nodes = lib.mapAttrs (_: node: { imports = [ node ./curl-cookie-jar.nix ]; }) {
    n1 = { ... }: nodeCommon 1;
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
    # The external client: a plain machine on the same LAN, no expanse.
    # Named n9 so the driver's name-sorted eth1 assignment leaves
    # 192.168.1.1-.3 for the cluster nodes (cluster-common.py hardcodes
    # n1=192.168.1.1); mirrors ui-vip-failover.nix's own client node.
    n9 = { ... }: {
      virtualisation.memorySize = 1024;
      networking.firewall.enable = false;
      environment.systemPackages = [ pkgs.curl ];
    };
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./block-common.py}
    ${builtins.readFile ./python/ui_vertical_slice.py}
  '';
}
