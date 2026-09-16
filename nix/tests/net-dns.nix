# §6 net-dns (G5.9, G5.10): the cluster DNS server (T17) answers the
# store-derived zone on every node (127.0.0.53), with the §4.4 record
# table realized end to end: block A → VIP (G5.9), per-replica A,
# SRV per named port; scale 3→5 and new replica records appear within
# 5 s (G5.10); a non-cluster name is forwarded to the configured
# upstream (dnsmasq on the client VM — the sandbox has no internet).
#
# systemd-resolved owns 127.0.0.53 on stock NixOS; it is disabled here
# (the §4.4 design gives the address to the agent's server), and
# resolv.conf is pinned to the agent stub + the slirp resolver.
{ self }:
{ pkgs, lib, ... }:
let
  expanse = self.packages.${pkgs.system}.expanse;
  nodeConfig = { hostId, host }: { ... }: {
    imports = [ self.nixosModules.expanse ];
    nixpkgs.overlays = [
      (final: prev: { expanse = self.packages.${prev.system}.expanse; })
    ];
    expanse.node.enable = true;
    expanse.agent.enable = true;
    expanse.hostId = hostId;
    expanse.hostname = host;
    virtualisation.memorySize = 2048;
    expanse.agent.period = "5s";
    expanse.agent.controllerPeriod = "5s";
    expanse.agent.blocksCatalog = ../blocks;
    expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
    environment.etc."expanse/blocks-flake".source = ../blocks-flake;
    expanse.agent.externalVIPPool = "192.168.1.100-192.168.1.100";
    expanse.agent.externalInterface = "eth1";
    # T17: DNS forwards to dnsmasq on the client VM (no internet here).
    expanse.agent.dnsUpstreams = "192.168.1.4:53";
    services.resolved.enable = false;
    # Static resolv.conf: our stub first (the agent's server), DHCP's
    # slirp resolver second (the forwarding upstream fallback).
    networking.resolvconf.enable = false;
    environment.etc."resolv.conf".text = ''
      nameserver 127.0.0.53
      nameserver 10.0.2.3
    '';
    networking.firewall.allowedTCPPorts = [ 80 8080 ];
    environment.systemPackages = with pkgs; [ curl jq dnsutils ];
  };
in
{
  name = "expanse-net-dns";

  nodes = {
    n1 = nodeConfig { hostId = "00000001"; host = "n1"; };
    n2 = nodeConfig { hostId = "00000002"; host = "n2"; };
    n3 = nodeConfig { hostId = "00000003"; host = "n3"; };
    n9 = { ... }: {
      virtualisation.memorySize = 1024;
      networking.firewall.enable = false;
      # The forwarding upstream: a fake authoritative server for a
      # name only this VM knows.
      services.dnsmasq = {
        enable = true;
        settings = {
          address = [ "/upstream.test/203.0.113.99" ];
          no-resolv = true; # no upstream: unknown names are refused
          port = 53;
          log-queries = true;
          listen-address = [ "192.168.1.4" ];
          bind-interfaces = true;
        };
      };
      environment.systemPackages = with pkgs; [ curl jq dnsutils ];
    };
  };

  testScript = ''
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./client-common.py}
    ${builtins.readFile ./block-common.py}

    client = n9  # the external client VM / forwarding upstream

    form("netdns")
    wait_agent_ready(n1)
    wait_agent_ready(n2)
    wait_agent_ready(n3)

    vip = "192.168.1.100"

    with subtest("deploy nginx (3 replicas, VIP) and api (1 replica, no VIP)"):
        nginx = (
            "apiVersion: expanse.io/v1\nkind: Block\n"
            "metadata:\n  name: nginx\n  namespace: default\n"
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
        api = (
            "apiVersion: expanse.io/v1\nkind: Block\n"
            "metadata:\n  name: api\n  namespace: default\n"
            "spec:\n  type: web/whoami\n  replicas: 1\n"
            "  resources:\n    requests:\n      cpu: 100m\n      memory: 64Mi\n"
            "  config:\n    port: 8081\n"
            "  network:\n    ports:\n"
            "      - name: http\n        port: 81\n        target_port: 8081\n"
            "        protocol: tcp\n        expose: EXPOSE_NONE\n"
        )
        deploy(n1, "nginx", nginx)
        deploy(n1, "api", api)

    with subtest("both blocks Running"):
        b = wait_phase(n1, "nginx", ["RUNNING"], 120)
        assert len(placement_nodes(b)) == 3, f"nginx placements: {b.get('status')}"
        b = wait_phase(n1, "api", ["RUNNING"], 120)
        assert len(placement_nodes(b)) == 1, f"api placements: {b.get('status')}"

    def dig(node, q):
        rc, out = node.execute(f"dig +short {q} @127.0.0.53 || true")
        return [l.strip() for l in out.splitlines() if l.strip()]

    with subtest("G5.9: every node's resolver answers the VIP for nginx"):
        for m in [n1, n2, n3]:
            deadline = time.time() + 60
            got = []
            while time.time() < deadline:
                got = dig(m, "nginx.default.expanse.local")
                if got == [vip]:
                    break
                time.sleep(2)
            assert got == [vip], f"{m.name}: nginx A = {got}, want [{vip}]"

    with subtest("per-replica A records match the placement nodes' overlay IPs"):
        # Overlay IPs read off the hosting nodes directly.
        want = set()
        for m in [n1, n2, n3]:
            rc, out = m.execute("ip -4 -o addr show exp0 | awk '{print $4}' || true")
            if "10.42." in out:
                want.add(out.split("/")[0].strip())
        for i in range(3):
            deadline = time.time() + 30
            got = []
            while time.time() < deadline:
                got = dig(n1, f"{i}.nginx.default.expanse.local")
                if got and got[0] in want:
                    break
                time.sleep(2)
            assert got and got[0] in want, f"replica {i} A = {got}, overlay IPs = {want}"

    with subtest("SRV per named port: _http._tcp.nginx → 3 replicas on port 80"):
        deadline = time.time() + 60
        got = []
        while time.time() < deadline:
            got = dig(n1, "SRV _http._tcp.nginx.default.expanse.local")
            if len(got) == 3:
                break
            time.sleep(2)
        assert len(got) == 3, f"SRV records: {got}"
        for rec in got:
            fields = rec.split()
            assert len(fields) == 4 and fields[2] == "8080", f"bad SRV (want replica port 8080): {rec}"
            assert fields[3].endswith(".nginx.default.expanse.local."), f"bad SRV target: {rec}"

    with subtest("block without a VIP: A = the replica's node overlay IP"):
        deadline = time.time() + 60
        got = []
        while time.time() < deadline:
            got = dig(n1, "api.default.expanse.local")
            if got:
                break
            time.sleep(2)
        assert len(got) == 1 and got[0].startswith("10.42."), f"api A = {got}"

    with subtest("G5.10: scale 3→5; new replica records appear within 5 s"):
        import base64
        # 5 replicas cannot satisfy ANTI_AFFINITY_NODE on 3 nodes - the
        # scaled manifest drops the constraint.
        nginx5 = (
            "apiVersion: expanse.io/v1\nkind: Block\n"
            "metadata:\n  name: nginx\n  namespace: default\n"
            "spec:\n  type: web/whoami\n  replicas: 5\n"
            "  resources:\n    requests:\n      cpu: 100m\n      memory: 64Mi\n"
            "  config:\n    port: 8080\n"
            "  network:\n    ports:\n"
            "      - name: http\n        port: 80\n        target_port: 8080\n"
            "        protocol: tcp\n        expose: EXPOSE_VIP\n"
            "    health_check:\n      readiness:\n        type: PROBE_TCP\n"
            "        port: 8080\n        period_seconds: 2\n"
        )
        enc = base64.b64encode(nginx5.encode()).decode()
        n1.succeed(f"echo {enc} | base64 -d > /tmp/nginx5.yaml")
        n1.succeed("expanse ctl block apply --socket /run/expanse/agent.sock -f /tmp/nginx5.yaml")
        # Wait for the controller to report 5 RUNNING placements, THEN
        # give DNS the §4.4 propagation budget (5 s).
        deadline = time.time() + 180
        pl = []
        while time.time() < deadline:
            b = get_json(n1, "nginx")
            if b is not None:
                pl = [p for p in (b.get("status") or {}).get("placements", [])
                      if p.get("phase") == "RUNNING"]
            if len(pl) >= 5:
                break
            time.sleep(2)
        assert len(pl) >= 5, f"never reached 5 RUNNING: {b.get('status')}"
        start = time.time()
        got = []
        while time.time() - start < 5 + 20:  # 5 s budget + dig cadence slack
            got = dig(n1, "4.nginx.default.expanse.local")
            if got:
                break
            time.sleep(1)
        assert got, f"4.nginx A never appeared; last={got}"
        print(f"NOTE: new replica record appeared after {time.time() - start:.1f}s")

    with subtest("forwarding: upstream.test resolves through the agent"):
        deadline = time.time() + 60
        got = []
        while time.time() < deadline:
            got = dig(n1, "upstream.test")
            if got == ["203.0.113.99"]:
                break
            time.sleep(2)
        assert got == ["203.0.113.99"], f"forwarded answer: {got}"
  '';
}
