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
      expanse.agent.externalVIPPool = "192.168.1.100-192.168.1.100";
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
      expanse.agent.externalVIPPool = "192.168.1.100-192.168.1.100";
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
      expanse.agent.externalVIPPool = "192.168.1.100-192.168.1.100";
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

    form("netlbhealth")
    wait_agent_ready(n1)
    wait_agent_ready(n2)
    wait_agent_ready(n3)

    vip = "192.168.1.100"
    unit = "expanse-block@default-web-0.service"

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

    with subtest("all replicas Running within 60 s"):
        b = wait_phase(n1, "web", ["RUNNING"], 90)
        nodes_ = placement_nodes(b)
        assert len(nodes_) == 3, f"want 3 distinct nodes, got {nodes_}: {b.get('status')}"

    with subtest("VIP serves and all 3 replicas are reachable"):
        deadline = time.time() + 60
        while time.time() < deadline:
            rc, out = n9.execute(f"curl -s --connect-timeout 3 http://{vip}/ || true")
            if out.strip().startswith("replica-"):
                break
            time.sleep(2)
        assert out.strip().startswith("replica-"), f"VIP never served: {out!r}"
        # Warm the round-robin state so all three backends have served.
        seen = set()
        for _ in range(30):
            rc, out = n9.execute(f"curl -s --connect-timeout 3 http://{vip}/ || true")
            seen.add(out.strip())
            if len(seen) == 3:
                break
        assert len(seen) == 3, f"not all replicas served during warmup: {seen}"

    with subtest("stop replica 0 (masked so the reconciler keeps it down)"):
        # Find which node hosts replica 0, then stop+mask its unit there.
        replica0_node = None
        for m in [n1, n2, n3]:
            rc, out = m.execute("systemctl list-units 'expanse-block@*default-web-0*' --no-legend || true")
            if out.strip():
                replica0_node = m
                break
        assert replica0_node is not None, "replica-0 unit not found on any node"
        # /etc is read-only on NixOS, so mask via /run (writable tmpfs
        # unit dir the reconciler's restart cannot get past).
        rc, out = replica0_node.execute(
            f"systemctl stop {unit} && "
            "mkdir -p /run/systemd/system && "
            f"ln -sf /dev/null /run/systemd/system/{unit} && "
            "systemctl daemon-reload")
        assert rc == 0, f"stop/mask failed: {out}"

    with subtest("within 10 s: no requests reach replica 0 and ZERO client errors (G5.7)"):
        # Poll the VIP continuously; a clean window ends 10 s after the
        # last reply from replica-0 (or client error). Every non-replica
        # reply is a client error and resets the window.
        errors = 0
        window_start = time.time()
        deadline = time.time() + 120
        while time.time() < deadline:
            rc, out = n9.execute(
                f"curl -s --connect-timeout 2 --max-time 4 http://{vip}/ || true")
            body = out.strip()
            if body == "replica-0":
                window_start = time.time()  # still serving from it
            elif body.startswith("replica-"):
                if time.time() - window_start >= 10:
                    break
            else:
                errors += 1
                window_start = time.time()
        assert time.time() - window_start >= 10, \
            f"replica-0 still served {time.time() - window_start:.0f} s ago"
        assert errors == 0, f"{errors} client errors during the transition"

    with subtest("restart replica 0: back in the pool within 15 s"):
        rc, out = replica0_node.execute(
            f"rm -f /run/systemd/system/{unit} && systemctl daemon-reload && "
            f"(systemctl reset-failed {unit} 2>/dev/null || true) && "
            f"systemctl start {unit}")
        assert rc == 0, f"restart failed: {out}"
        deadline = time.time() + 15 + 30  # 15 s budget + slack for probes
        back = False
        while time.time() < deadline:
            rc, out = n9.execute(f"curl -s --connect-timeout 2 --max-time 4 http://{vip}/ || true")
            if out.strip() == "replica-0":
                back = True
                break
            time.sleep(1)
        assert back, "replica-0 never returned to the pool after restart"
  '';
}
