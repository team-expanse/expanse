# §6 net-lb-distribution (G5.6): deploy web/whoami replicas=3 with
# expose: vip; 1000 requests through the VIP; all 3 replica indices
# appear and each is within ±20% of even (round-robin fan-out across
# the cluster).
{ self }:
{ pkgs, lib, ... }:
let
  expanse = self.packages.${pkgs.system}.expanse;
in
{
  name = "expanse-net-lb-distribution";

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
      expanse.agent.externalVIPPool = "192.168.1.100-192.168.1.101";
      expanse.agent.externalInterface = "eth1";
      networking.firewall.allowedTCPPorts = [ 80 8080 ];
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
      expanse.agent.externalVIPPool = "192.168.1.100-192.168.1.101";
      expanse.agent.externalInterface = "eth1";
      networking.firewall.allowedTCPPorts = [ 80 8080 ];
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
      expanse.agent.externalVIPPool = "192.168.1.100-192.168.1.101";
      expanse.agent.externalInterface = "eth1";
      networking.firewall.allowedTCPPorts = [ 80 8080 ];
    };
    # External client: named n9 so the name-sorted eth1 assignment
    # leaves 192.168.1.1-.3 for the cluster nodes.
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

    form("netlbdist")
    wait_agent_ready(n1)
    wait_agent_ready(n2)
    wait_agent_ready(n3)


    with subtest("deploy whoami replicas=3 with expose: vip"):
        manifest = (
            "apiVersion: expanse.io/v1\nkind: Block\n"
            "metadata:\n  name: web\n  namespace: default\n"
            "spec:\n  type: web/whoami\n  replicas: 3\n"
            "  placement:\n    antiAffinity: ANTI_AFFINITY_NODE\n"
            "  resources:\n    requests:\n      cpu: 100m\n      memory: 64Mi\n"
            "  config:\n    port: 8080\n"
            "  network:\n    ports:\n"
            "      - name: http\n        port: 80\n        target_port: 8080\n"
            "        protocol: tcp\n        expose: EXPOSE_VIP\n"
            "    health_check:\n      readiness:\n        type: PROBE_TCP\n"
            "        port: 8080\n        period_seconds: 2\n"
        )
        deploy(n1, "web", manifest)
        vip = wait_block_vip(n1, "web")

    with subtest("all replicas Running within 60 s"):
        b = wait_phase(n1, "web", ["RUNNING"], 90)
        nodes_ = placement_nodes(b)
        assert len(nodes_) == 3, f"want 3 distinct nodes, got {nodes_}: {b.get('status')}"

    with subtest("VIP serves through the LB"):
        deadline = time.time() + 60
        out = ""
        while time.time() < deadline:
            rc, out = n9.execute(
                f"curl -s --connect-timeout 3 http://{vip}/ || true")
            if out.strip().startswith("replica-"):
                break
            time.sleep(2)
        assert out.strip().startswith("replica-"), f"VIP never served a replica: {out!r}"

    with subtest("1000 requests: all 3 indices, within ±20% of even (G5.6)"):
        rc, counts = n9.execute(
            "for i in $(seq 1000); do curl -s --connect-timeout 3 --max-time 5 "
            f"http://{vip}/; done | sort | uniq -c")
        print("distribution:", counts)
        # Parse "<count> replica-<n>" lines.
        seen = {}
        for line in counts.strip().splitlines():
            parts = line.split()
            if len(parts) == 2:
                seen[parts[1]] = int(parts[0])
        assert set(seen.keys()) == {"replica-0", "replica-1", "replica-2"}, \
            f"not all replicas served: {seen}"
        for name, n in seen.items():
            want = 1000 / 3
            lo, hi = want * 0.8, want * 1.2  # ±20% of even (266..400)
            assert lo <= n <= hi, f"{name} served {n} times, outside [{lo:.0f},{hi:.0f}]"
  '';
}
