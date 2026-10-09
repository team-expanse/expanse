# monitor/uptime-kuma: a SINGLETON Uptime Kuma on a DRBD-backed volume behind a VIP on 80.
# The serving node is crashed right after monitors are added and the survivor
# must have them. The scenario is python/monitor_uptime_kuma.py.
{ self }:
{ pkgs, lib, ... }:
let
  lint = pkgs.runCommand "monitor-uptime-kuma-lint" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    export PYTHONDONTWRITEBYTECODE=1
    for f in ${./cluster-common.py} ${./block-common.py} ${./python/vol_cluster.py} ${./python/uptime_kuma_client.py} ${./python/monitor_uptime_kuma.py}; do
      python3 -c 'import sys; compile(open(sys.argv[1]).read(), sys.argv[1], "exec")' $f
    done
    touch $out
  '';
  # The socket.io client the scenario drives Uptime Kuma with, as its web UI does.
  kuma = pkgs.writeShellScriptBin "kuma" ''
    exec ${pkgs.python3.withPackages (p: [ p.python-socketio p.websocket-client p.requests ])}/bin/python3 \
      ${./python/uptime_kuma_client.py} "$@"
  '';
  # The management UI always takes one pool address (share-smb.nix's note).
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
    # The NixOS firewall is on; the agent's own nftables ruleset is opt-in.
    networking.firewall.allowedTCPPorts = [ 80 ];
    # Binary-backed blocks exec upstream binaries from PATH (block-catalog.nix).
    environment.systemPackages = [ pkgs.curl pkgs.jq pkgs.uptime-kuma ];
    virtualisation.memorySize = 1536;
  };
in
{
  name = "expanse-monitor-uptime-kuma";

  nodes = {
    n1 = { ... }: nodeCommon 1;
    n2 = { ... }: nodeCommon 2;
    n3 = { ... }: nodeCommon 3;
    # External client and monitored web server, named n9 so the cluster keeps 192.168.1.1-.3.
    n9 = { ... }: {
      virtualisation.memorySize = 1024;
      networking.firewall.enable = false;
      environment.systemPackages = [ pkgs.curl kuma ];
      systemd.services.monitored = {
        wantedBy = [ "multi-user.target" ];
        serviceConfig.ExecStart = "${pkgs.python3}/bin/python3 -m http.server 8080 --directory /etc";
      };
    };
  };

  testScript = ''
    # ${lint}
    ${builtins.readFile ./cluster-common.py}
    client = n9
    ${builtins.readFile ./block-common.py}
    ${builtins.readFile ./python/vol_cluster.py}
    ${builtins.readFile ./python/monitor_uptime_kuma.py}
  '';
}
