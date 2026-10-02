# §8 M3 demo, as a repeatable VM test (the planner's choice over a
# recorded terminal session — a check that runs in CI can never go
# stale). The narrative: deploy nginx on a 3-node cluster, curl the VIP
# from an external client, hard power off one node, show curl keeps
# working. This is the same choreography as net-vip-failover.nix but
# with demo narration and no budget assertions — it exists to be shown,
# net-vip-failover.nix remains the strict measurement.
{ self }:
{ pkgs, lib, ... }:
let
  expanse = self.packages.${pkgs.system}.expanse;
in
{
  name = "expanse-m3-demo";

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
      networking.firewall.allowedTCPPorts = [ 80 8080 7443 7444 7445 7446 ];
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
      expanse.agent.externalVIPPool = "192.168.1.100-192.168.1.101";
      expanse.agent.externalInterface = "eth1";
      networking.firewall.allowedTCPPorts = [ 80 8080 7443 7444 7445 7446 ];
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
      expanse.agent.externalVIPPool = "192.168.1.100-192.168.1.101";
      expanse.agent.externalInterface = "eth1";
      networking.firewall.allowedTCPPorts = [ 80 8080 7443 7444 7445 7446 ];
      environment.systemPackages = with pkgs; [ openssl curl jq nginx ];
    };
    # The external client: a plain machine on the same LAN, no expanse.
    # Named "n9" so the driver's name-sorted eth1 assignment leaves
    # 192.168.1.1-.3 for the cluster nodes (client-common.py note).
    n9 = { ... }: {
      virtualisation.memorySize = 1024;
      networking.firewall.enable = false;
      environment.systemPackages = with pkgs; [ curl iputils ];
    };
  };

  testScript = ''
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./client-common.py}

    client = n9
    ${builtins.readFile ./block-common.py}

    form("m3demo")
    wait_agent_ready(n1)
    wait_agent_ready(n2)
    wait_agent_ready(n3)

    print("=== M3 DEMO: node failure does not take the service down ===")

    with subtest("act 1 — deploy a 3-replica web service with a VIP"):
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
        vip = wait_block_vip(n1, "web")
        wait_phase(n1, "web", ["RUNNING"], 60)
        print(f"web deployed: 3 replicas across n1/n2/n3, VIP {vip} announced")

    with subtest("act 2 — the outside world sees the service"):
        deadline = time.time() + 30
        out = ""
        while time.time() < deadline:
            rc, out = n9.execute(f"curl -s -o /dev/null -w '%{{http_code}}' --connect-timeout 3 http://{vip}/ || true")
            if out.strip() == "200":
                break
            time.sleep(2)
        assert out.strip() == "200", f"client curl to {vip} never returned 200 (last: {out!r})"
        print(f"external client: curl http://{vip}/ -> 200")

    # Keep traffic flowing through the whole demo (the M3 script has the
    # client curling in a loop at ~20 req/s) — same warm path
    # net-vip-failover.nix measures against.
    loop = (
        "nohup bash -c 'end=$((SECONDS+75)); "
        "while [ $SECONDS -lt $end ]; do "
        "ts=$(date +%s.%N); "
        "code=$(curl -s -o /dev/null -w %{http_code} --connect-timeout 2 http://" + vip + "/); "
        "echo \"$ts $code\" >> /tmp/curl-out.log; sleep 0.05; done' "
        ">/dev/null 2>&1 &"
    )
    n9.execute("rm -f /tmp/curl-out.log")
    n9.execute(loop, check_return=False)
    time.sleep(3)  # let the loop record some 200s first

    with subtest("act 3 — hard power off the VIP holder (the unplugged cable)"):
        holder = None
        for m in [n1, n2, n3]:
            rc, out = m.execute(f"ip -4 -o addr show eth1 | grep -F {vip} || true")
            if rc == 0 and out.strip():
                holder = m
                break
        assert holder is not None, "no node holds the VIP"
        print(f"VIP holder is {holder.name}; powering it off NOW")
        holder.send_monitor_command("stop")

    with subtest("act 4 — curl keeps working (failover, G5.4 budget enforced by net-vip-failover)"):
        # The ≤ 15 s G5.4 budget is MEASURED by net-vip-failover.nix
        # (10.5 s with the 2 s raft RPC timeout + 10 s VIP lease TTL).
        # This demo only tells the story, so it tolerates the rare
        # post-freeze election stall (up to ~20 s observed) and prints
        # the measured time instead of budget-enforcing it.
        t0 = time.time()
        recovered = None
        while time.time() - t0 < 40:
            rc, out = n9.execute(f"curl -s -o /dev/null -w '%{{http_code}}' --connect-timeout 2 http://{vip}/ || true")
            if out.strip() == "200":
                recovered = time.time() - t0
                break
            time.sleep(0.3)
        assert recovered is not None, "curl never recovered after power-off"
        if recovered > 15:
            print(f"WARNING: recovery took {recovered:.1f}s — G5.4 budget is 15s; net-vip-failover measures this")        # The VIP must be on exactly one surviving node (no duplicate).
        holders = []
        deadline = time.time() + 30
        while time.time() < deadline and len(holders) != 1:
            holders = []
            for m in [n1, n2, n3]:
                if m is holder:
                    continue
                rc, out = m.execute(f"ip -4 -o addr show eth1 | grep -F {vip} || true")
                if rc == 0 and out.strip():
                    holders.append(m.name)
            time.sleep(2)
        assert len(holders) == 1, f"VIP on {holders}, want exactly one survivor"
        print(f"failover complete in {recovered:.1f}s: {holder.name} -> {holders[0]}; client never noticed")

    with subtest("act 5 — power it back on, service stays put (no flapping)"):
        holder.send_monitor_command("cont")
        time.sleep(20)
        rc, out = holder.execute(f"ip -4 -o addr show eth1 | grep -F {vip} || true")
        assert not (rc == 0 and out.strip()), "VIP flapped back to the restored node"
        for _ in range(5):
            rc, out = n9.execute(f"curl -s -o /dev/null -w '%{{http_code}}' --connect-timeout 3 http://{vip}/ || true")
            assert out.strip() == "200", f"client got {out!r} after restore"
            time.sleep(0.3)
        print(f"{holder.name} rejoined; VIP stayed on {holders[0]}; client still gets 200")
        print("=== M3 DEMO PASSED ===")
  '';
}
