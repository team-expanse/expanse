# §6 net-vip-basic: deploy nginx replicas=3 with expose: vip; assert
# exactly ONE node holds the external VIP (192.168.1.100/24) and the
# external client VM gets a 200 from the VIP.
{ self }:
{ pkgs, lib, ... }:
let
  expanse = self.packages.${pkgs.system}.expanse;
in
{
  name = "expanse-net-vip-basic";

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
      # §4.2 external pool: a single address keeps the test deterministic.
      expanse.agent.externalVIPPool = "192.168.1.100-192.168.1.100";
      # The test LAN rides eth1 (the driver's 192.168.1.0/24 segment);
      # "auto" would resolve to the NAT'd eth0 default route.
      expanse.agent.externalInterface = "eth1";
      networking.firewall.allowedTCPPorts = [ 80 8080 7443 7444 7445 7446 ];
      # Binary-backed blocks exec upstream binaries from the system
      # profile (block-catalog.nix convention).
      environment.systemPackages = with pkgs; [ openssl curl jq nginx ];
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
      expanse.agent.externalVIPPool = "192.168.1.100-192.168.1.100";
      expanse.agent.externalInterface = "eth1";
      networking.firewall.allowedTCPPorts = [ 80 8080 7443 7444 7445 7446 ];
      # Binary-backed blocks exec upstream binaries from the system
      # profile (block-catalog.nix convention).
      environment.systemPackages = with pkgs; [ openssl curl jq nginx ];
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
      expanse.agent.externalVIPPool = "192.168.1.100-192.168.1.100";
      expanse.agent.externalInterface = "eth1";
      networking.firewall.allowedTCPPorts = [ 80 8080 7443 7444 7445 7446 ];
      # Binary-backed blocks exec upstream binaries from the system
      # profile (block-catalog.nix convention).
      environment.systemPackages = with pkgs; [ openssl curl jq nginx ];
    };
    # The external client: a plain machine on the same LAN, no expanse.
    # Named "n9" so the driver's name-sorted eth1 assignment leaves
    # 192.168.1.1-.3 for the cluster nodes (cluster-common.py hardcodes
    # n1=192.168.1.1).
    n9 = { ... }: {
      virtualisation.memorySize = 1024;
      networking.firewall.enable = false;
      environment.systemPackages = with pkgs; [ curl iputils ];
    };
  };

  testScript = ''
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./client-common.py}

    client = n9  # the external client VM
    ${builtins.readFile ./block-common.py}

    form("netvip")
    wait_agent_ready(n1)
    wait_agent_ready(n2)
    wait_agent_ready(n3)

    vip = "192.168.1.100"

    with subtest("deploy nginx replicas=3 with expose: vip"):
        manifest = (
            "apiVersion: expanse.io/v1\nkind: Block\n"
            "metadata:\n  name: web\n  namespace: default\n"
            "spec:\n  type: web/nginx\n  replicas: 3\n"
            "  placement:\n    antiAffinity: ANTI_AFFINITY_NODE\n"
            "  resources:\n    requests:\n      cpu: 100m\n      memory: 64Mi\n"
            "  config:\n    port: 8080\n    serverName: web\n"
            "  network:\n    ports:\n"
            "      - name: http\n        port: 80\n        target_port: 8080\n"
            "        protocol: tcp\n        expose: EXPOSE_VIP\n"
            "    health_check:\n      readiness:\n        type: PROBE_TCP\n"
            "        port: 8080\n        period_seconds: 2\n"
        )
        deploy(n1, "web", manifest)

    with subtest("all replicas Running within 30 s (G4.5)"):
        b = wait_phase(n1, "web", ["RUNNING"], 60)
        nodes_ = placement_nodes(b)
        assert len(nodes_) == 3, f"want 3 distinct nodes, got {nodes_}: {b.get('status')}"

    with subtest("exactly one node holds the VIP"):
        # Allow 30 s: allocation + lease acquisition + announce.
        deadline = time.time() + 30
        holders = []
        while time.time() < deadline:
            holders = []
            for m in [n1, n2, n3]:
                rc, out = m.execute(f"ip -4 -o addr show eth1 | grep -F {vip} || true")
                if rc == 0 and out.strip():
                    holders.append(m.name)
            if len(holders) == 1:
                break
            time.sleep(2)
        assert len(holders) == 1, f"want exactly 1 VIP holder, got {holders}"

    with subtest("external client gets 200 from the VIP"):
        deadline = time.time() + 30
        code = ""
        while time.time() < deadline:
            rc, out = n9.execute(f"curl -s -o /dev/null -w '%{{http_code}}' --connect-timeout 3 http://{vip}/ || true")
            if out.strip() == "200":
                code = "200"
                break
            time.sleep(2)
        assert code == "200", f"client curl to {vip} never returned 200 (last: {out!r})"

    with subtest("client still gets 200 (10 requests)"):
        for _ in range(10):
            rc, out = n9.execute(
                f"curl -s -o /dev/null -w '%{{http_code}}' --connect-timeout 3 http://{vip}/ || true")
            assert out.strip() == "200", f"client got {out!r}"
            time.sleep(0.3)
  '';
}
