# §6 net-lb-health (G5.7): with a 3-replica whoami block behind the
# VIP, stop one replica's unit (and mask it so the reconciler keeps it
# down); within 10 s no further requests reach it and the client sees
# ZERO errors (health-gated pool + connect-error rotation); restart
# it; it is back in the pool within 15 s.
{ self }:
{ pkgs, lib, ... }:
let
  expanse = self.packages.${pkgs.system}.expanse;
in
{
  name = "expanse-net-lb-health";

  nodes = {
    n1 = { ... }: {
      imports = [ self.nixosModules.expanse ];
      nixpkgs.overlays = [
        (final: prev: { expanse = self.packages.${prev.system}.expanse; })
      ];
      expanse.node.enable = true;
      expanse.agent.enable = true;
      expanse.hostId = "00000001";
      expanse.hostname = "n1";
      virtualisation.memorySize = 2048;
      expanse.agent.period = "5s";
      expanse.agent.controllerPeriod = "5s";
      expanse.agent.blocksCatalog = ../blocks;
      expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
      environment.etc."expanse/blocks-flake".source = ../blocks-flake;
      expanse.agent.externalVIPPool = "192.168.1.100-192.168.1.101"; # the web UI's own VIP takes one
      expanse.agent.externalInterface = "eth1";
      networking.firewall.allowedTCPPorts = [ 80 8080 ];
      environment.systemPackages = with pkgs; [ curl jq ];
    };
    n2 = { ... }: {
      imports = [ self.nixosModules.expanse ];
      nixpkgs.overlays = [
        (final: prev: { expanse = self.packages.${prev.system}.expanse; })
      ];
      expanse.node.enable = true;
      expanse.agent.enable = true;
      expanse.hostId = "00000002";
      expanse.hostname = "n2";
      virtualisation.memorySize = 2048;
      expanse.agent.period = "5s";
      expanse.agent.controllerPeriod = "5s";
      expanse.agent.blocksCatalog = ../blocks;
      expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
      environment.etc."expanse/blocks-flake".source = ../blocks-flake;
      expanse.agent.externalVIPPool = "192.168.1.100-192.168.1.101"; # the web UI's own VIP takes one
      expanse.agent.externalInterface = "eth1";
      networking.firewall.allowedTCPPorts = [ 80 8080 ];
      environment.systemPackages = with pkgs; [ curl jq ];
    };
    n3 = { ... }: {
      imports = [ self.nixosModules.expanse ];
      nixpkgs.overlays = [
        (final: prev: { expanse = self.packages.${prev.system}.expanse; })
      ];
      expanse.node.enable = true;
      expanse.agent.enable = true;
      expanse.hostId = "00000003";
      expanse.hostname = "n3";
      virtualisation.memorySize = 2048;
      expanse.agent.period = "5s";
      expanse.agent.controllerPeriod = "5s";
      expanse.agent.blocksCatalog = ../blocks;
      expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
      environment.etc."expanse/blocks-flake".source = ../blocks-flake;
      expanse.agent.externalVIPPool = "192.168.1.100-192.168.1.101"; # the web UI's own VIP takes one
      expanse.agent.externalInterface = "eth1";
      networking.firewall.allowedTCPPorts = [ 80 8080 ];
      environment.systemPackages = with pkgs; [ curl jq ];
    };
    n9 = { ... }: {
      virtualisation.memorySize = 1024;
      networking.firewall.enable = false;
      environment.systemPackages = with pkgs; [ curl jq ];
    };
  };

  testScript = ''
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./client-common.py}

    client = n9  # the external client VM
    ${builtins.readFile ./block-common.py}
    ${builtins.readFile ./python/net_lb_health.py}
  '';
}
