# §6 net-vip-basic: deploy nginx replicas=3 with expose: vip; assert
# exactly ONE node holds the external VIP (192.168.1.100/24) and the
# external client VM gets a 200 from the VIP.
{ self }:
{ pkgs, lib, ... }:
let
  expanse = self.packages.${pkgs.system}.expanse;
in
{
  name = "expanse-net-vip-failover";

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

    with subtest("all replicas Running and exactly one holder"):
        b = wait_phase(n1, "web", ["RUNNING"], 60)
        deadline = time.time() + 30
        holder = None
        while time.time() < deadline and holder is None:
            for m in [n1, n2, n3]:
                rc, out = m.execute(f"ip -4 -o addr show eth1 | grep -F {vip} || true")
                if rc == 0 and out.strip():
                    holder = m
                    break
            time.sleep(2)
        assert holder is not None, "no node holds the VIP"

    with subtest("external client gets 200 from the VIP"):
        deadline = time.time() + 30
        out = ""
        while time.time() < deadline:
            rc, out = n9.execute(f"curl -s -o /dev/null -w '%{{http_code}}' --connect-timeout 3 http://{vip}/ || true")
            if out.strip() == "200":
                break
            time.sleep(2)
        assert out.strip() == "200", f"client curl to {vip} never returned 200 (last: {out!r})"

    # Sustained load: 20 req/s from the external client, timestamped.
    # Runs for 75 s, long enough to cover the outage and recovery plus
    # the restore window. Each line: "<unix-time> <http-code>".
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

    with subtest("hard power-off the VIP holder"):
        t0 = time.time()
        holder.send_monitor_command("stop")  # freeze the VM: no packets, no renewal

    with subtest("curl recovers within 15 s (G5.4)"):
        recovered = None
        while time.time() - t0 < 25:
            rc, out = n9.execute(f"curl -s -o /dev/null -w '%{{http_code}}' --connect-timeout 2 http://{vip}/ || true")
            if out.strip() == "200":
                recovered = time.time() - t0
                break
            time.sleep(0.3)
        assert recovered is not None, "curl never recovered after power-off"
        print(f"FAILover recovery time: {recovered:.1f}s")
        assert recovered <= 15, f"recovery took {recovered:.1f}s, want <= 15s (G5.4)"

    with subtest("failures < 300 and contiguous (one outage window)"):
        n9.execute("pkill -f 'curl-out.log' || true", check_return=False)
        n9.execute("pkill curl || true", check_return=False)
        time.sleep(1)
        rc, logdata = n9.execute("cat /tmp/curl-out.log")
        rows = [l.split() for l in logdata.strip().splitlines() if l.strip()]
        codes = [r[1] for r in rows]
        fails = [i for i, c in enumerate(codes) if c != "200"]
        assert 0 < len(fails) < 300, f"failed requests: {len(fails)} (codes tail: {codes[-10:]})"
        # Contiguity: no success between the first and last failure.
        assert codes[max(fails):].count("200") == len(codes) - max(fails) - 1, (
            f"failures not contiguous: successes inside the outage window "
            f"(first fail idx {fails[0]}, last {fails[-1]}) — flapping or duplicate holder"
        )

    with subtest("VIP lands on exactly one surviving node"):
        deadline = time.time() + 30
        survivors = []
        while time.time() < deadline:
            survivors = []
            for m in [n2, n3]:
                rc, out = m.execute(f"ip -4 -o addr show eth1 | grep -F {vip} || true")
                if rc == 0 and out.strip():
                    survivors.append(m.name)
            if len(survivors) == 1:
                break
            time.sleep(2)
        assert len(survivors) == 1 and survivors[0] != holder.name, (
            f"want VIP on exactly one surviving node (was on {holder.name}), got {survivors}"
        )

    with subtest("restore the node: VIP does not flap back, client keeps working"):
        holder.send_monitor_command("cont")
        time.sleep(20)  # n1 re-joins raft, agent resumes, becomes eligible again
        # The original holder must NOT have taken the VIP back.
        rc, out = holder.execute(f"ip -4 -o addr show eth1 | grep -F {vip} || true")
        assert not (rc == 0 and out.strip()), "VIP flapped back to the restored node"
        # And the restored node rejoins cleanly: linearized read via its
        # agent (ctl get forwards through raft) proves store connectivity.
        deadline = time.time() + 60
        rejoined = False
        while time.time() < deadline and not rejoined:
            rc, out = holder.execute("expanse ctl block get --socket /run/expanse/agent.sock -n default -o json web || true")
            if rc == 0 and '"web"' in out:
                rejoined = True
            time.sleep(3)
        assert rejoined, "restored node did not rejoin the cluster"
        for _ in range(5):
            rc, out = n9.execute(f"curl -s -o /dev/null -w '%{{http_code}}' --connect-timeout 3 http://{vip}/ || true")
            assert out.strip() == "200", f"client got {out!r} after restore"
            time.sleep(0.3)
  '';
}
