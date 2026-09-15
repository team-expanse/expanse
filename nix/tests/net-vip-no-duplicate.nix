# §6 net-vip-no-duplicate (safety-critical, G5.5): deploy a VIP, run an
# ARP checker on the external client (arping -c1 every 200 ms, recording
# the responding MAC), and drive a partition storm — randomly isolate one
# node (nftables drop both directions, holder included) for 10–30 s, 40
# times. Invariants:
#   (a) the client never observes split brain: MAC flip intervals are
#       never shorter than a legal failover cycle (two flips < 12 s apart
#       is impossible without two nodes answering at once — a legal
#       flip needs the old holder to lose its lease and the survivor's
#       takeover at lease expiry), and
#   (b) `ip addr` polled every 200 ms on all nodes never shows the VIP
#       on 2 nodes within a 2 s window (legit failover has a ≥ 10 s
#       gap: release happens on renew failure, takeover at TTL expiry).
# Zero duplicate observations across all 40 partitions is the §8 exit
# gate. Per Phase04 T25 precedent: a failure here is a real bug, not a
# timing fudge.
{ self }:
{ pkgs, lib, ... }:
let
  expanse = self.packages.${pkgs.system}.expanse;
in
{
  name = "expanse-net-vip-no-duplicate";

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
      expanse.agent.externalVIPPool = "192.168.1.100-192.168.1.100";
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
      expanse.agent.externalVIPPool = "192.168.1.100-192.168.1.100";
      expanse.agent.externalInterface = "eth1";
      networking.firewall.allowedTCPPorts = [ 80 8080 7443 7444 7445 7446 ];
      environment.systemPackages = with pkgs; [ openssl curl jq nginx ];
    };
    # The external client: a plain machine on the same LAN, no expanse.
    # Named "n9" so the driver's name-sorted eth1 assignment leaves
    # 192.168.1.1-.3 for the cluster nodes (cluster-common.py hardcodes
    # n1=192.168.1.1).
    n9 = { ... }: {
      virtualisation.memorySize = 1024;
      networking.firewall.enable = false;
      environment.systemPackages = with pkgs; [ curl iputils arping ];
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
    ips = { "n1": "192.168.1.1", "n2": "192.168.1.2", "n3": "192.168.1.3" }
    nodes = [n1, n2, n3]

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
            for m in nodes:
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

    # Continuous observers, started once and killed after the storm.
    # ARP checker on the client: one arping -c1 per ~200 ms window; each
    # line is "<unix-time> <mac-or-empty>". The partition rules drop
    # node-to-node traffic only, so a partitioned holder KEEPS answering
    # the client's broadcasts until its own renewal fails — maximal
    # duplicate pressure.
    n9.execute("rm -f /tmp/arp.log", check_return=False)
    arp_loop = (
        "nohup bash -c 'end=$((SECONDS+2400)); "
        "while [ $SECONDS -lt $end ]; do "
        "mac=$(timeout 2 arping -c1 -I eth1 " + vip + " 2>/dev/null | "
        "grep -oE \"[0-9a-fA-F]{2}(:[0-9a-fA-F]{2}){5}\" | head -1); "
        "echo \"$(date +%s.%N) $mac\" >> /tmp/arp.log; "
        "sleep 0.2; done' >/dev/null 2>&1 &"
    )
    n9.execute(arp_loop, check_return=False)

    # VIP-presence pollers: every 200 ms, if the node has the VIP, log
    # the timestamp.
    for m in nodes:
        m.execute(f"rm -f /tmp/viplog-{m.name}", check_return=False)
        m.execute(
            "nohup bash -c 'end=$((SECONDS+2400)); "
            "while [ $SECONDS -lt $end ]; do "
            f"ip -4 -o addr show eth1 | grep -qF {vip} && date +%s.%N >> /tmp/viplog-{m.name}; "
            "sleep 0.2; done' >/dev/null 2>&1 &",
            check_return=False,
        )
    time.sleep(2)

    with subtest("partition storm: 40 random isolations, 10-30 s each"):
        import random
        random.seed(20250915)
        rounds = []
        for i in range(40):
            victim = random.choice(nodes)
            dur = random.randint(10, 30)
            others = [m for m in nodes if m is not victim]
            o1, o2 = ips[others[0].name], ips[others[1].name]
            victim.succeed(
                "nft add table ip exppart; "
                "nft add chain ip exppart output '{ type filter hook output priority 0; }'; "
                f"nft add rule ip exppart output ip daddr {o1} drop; "
                f"nft add rule ip exppart output ip daddr {o2} drop; "
                "nft add chain ip exppart input '{ type filter hook input priority 0; }'; "
                f"nft add rule ip exppart input ip saddr {o1} drop; "
                f"nft add rule ip exppart input ip saddr {o2} drop"
            )
            time.sleep(dur)
            victim.succeed("nft delete table ip exppart")
            # Heal: the victim must be able to read the (linearized)
            # store through its own agent before the next round.
            deadline = time.time() + 90
            ok = False
            while time.time() < deadline and not ok:
                rc, out = victim.execute(
                    "expanse ctl block get --socket /run/expanse/agent.sock -n default -o json web || true"
                )
                if rc == 0 and '"web"' in out:
                    ok = True
                else:
                    time.sleep(2)
            assert ok, f"round {i}: {victim.name} did not rejoin after heal"
            rounds.append((victim.name, dur))
            time.sleep(3)  # settle before the next isolation
        print("storm: " + ", ".join(f"{v}={d}s" for v, d in rounds))

    for m in nodes + [n9]:
        m.execute("pkill -f 'SECONDS+2400' || true", check_return=False)
    time.sleep(1)

    with subtest("invariant (a): never two distinct MACs answering at once"):
        rc, arplog = n9.execute("cat /tmp/arp.log")
        rows = [l.split() for l in arplog.strip().splitlines() if l.strip()]
        answered = [(float(r[0]), r[1]) for r in rows if len(r) > 1 and r[1]]
        assert answered, "ARP checker recorded no replies at all"
        # Compress into runs of identical MACs; each run boundary is a
        # MAC flip (a failover). Legal flips are separated by at least
        # one full failover cycle (~lease TTL); a flip < 12 s after the
        # previous one means two nodes were answering simultaneously.
        flips = []
        prev = None
        for ts, mac in answered:
            if mac != prev:
                flips.append((ts, mac))
                prev = mac
        for (t1, m1), (t2, m2) in zip(flips, flips[1:]):
            assert t2 - t1 >= 12, (
                f"split brain: {m1} and {m2} both answered around t={t2:.1f} "
                f"({t2 - t1:.1f}s after the previous flip) — duplicate holder"
            )
        print(f"ARP checker: {len(answered)} replies, {len(flips)} MAC flips, "
              f"sequence: {' -> '.join(m for _, m in flips)}")

    with subtest("invariant (b): VIP never present on 2 nodes simultaneously"):
        logs = {}
        for m in nodes:
            rc, out = m.execute(f"cat /tmp/viplog-{m.name}")
            logs[m.name] = [float(x) for x in out.split() if x.strip()]
        names = sorted(logs)
        for i in range(len(names)):
            for j in range(i + 1, len(names)):
                a, b = logs[names[i]], logs[names[j]]
                # Two-sample overlap within 2 s: legal failover leaves a
                # >= 10 s gap (release on renew failure vs takeover at
                # TTL expiry), so any 2 s proximity is a real overlap.
                overlap = None
                ai = bi = 0
                while ai < len(a) and bi < len(b):
                    if abs(a[ai] - b[bi]) < 2.0:
                        overlap = (a[ai], b[bi])
                        break
                    if a[ai] < b[bi]:
                        ai += 1
                    else:
                        bi += 1
                assert overlap is None, (
                    f"VIP on both {names[i]} (t={overlap[0]:.1f}) and "
                    f"{names[j]} (t={overlap[1]:.1f}) — duplicate holder"
                )
        total = sum(len(v) for v in logs.values())
        print(f"VIP-presence samples: {total} across 3 nodes, zero overlaps")
  '';
}
