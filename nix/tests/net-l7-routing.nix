# §6 net-l7-routing (G5.8): three blocks behind ONE external VIP —
# `edge` declares the L7 routes; `a` and `b` are the upstreams.
# Assert Host-based routing (a.test.local vs b.test.local), path-prefix
# routing (/api on b.test.local → block b), no-route → 404, and that
# X-Forwarded-For carries the real external client IP.
{ self }:
{ pkgs, lib, ... }:
let
  expanse = self.packages.${pkgs.system}.expanse;
in
{
  name = "expanse-net-l7-routing";

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
      expanse.agent.externalVIPPool = "192.168.1.100-192.168.1.102";
      expanse.agent.externalInterface = "eth1";
      networking.firewall.allowedTCPPorts = [ 80 8080 8081 8082 ];
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
      expanse.agent.externalVIPPool = "192.168.1.100-192.168.1.102";
      expanse.agent.externalInterface = "eth1";
      networking.firewall.allowedTCPPorts = [ 80 8080 8081 8082 ];
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
      expanse.agent.externalVIPPool = "192.168.1.100-192.168.1.102";
      expanse.agent.externalInterface = "eth1";
      networking.firewall.allowedTCPPorts = [ 80 8080 8081 8082 ];
    };
    # External client: named n9 so the name-sorted eth1 assignment
    # leaves 192.168.1.1-.3 for the cluster nodes (n9 takes .4).
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

    form("netl7route")
    wait_agent_ready(n1)
    wait_agent_ready(n2)
    wait_agent_ready(n3)

    # Discover the edge block's VIP: the edge is the only listener
    # where Host b.test.local + /api routes to block b AND Host
    # b.test.local + / routes to block a (an upstream's own L4 would
    # answer b for both, so both conditions must hold). VIP allocation
    # order is store-scan order (a/b/edge by key sort), but the test
    # must not depend on it.
    def find_edge_vip():
        deadline = time.time() + 90
        while time.time() < deadline:
            for cand in ("192.168.1.100", "192.168.1.101", "192.168.1.102"):
                rc, api = n9.execute(
                    f"curl -s --connect-timeout 3 --max-time 5 "
                    f"-H 'Host: b.test.local' http://{cand}/api || true")
                if "b-replica-0" not in api:
                    continue
                rc, root = n9.execute(
                    f"curl -s --connect-timeout 3 --max-time 5 "
                    f"-H 'Host: b.test.local' http://{cand}/ || true")
                if "a-replica-0" in root:
                    return cand
            time.sleep(2)
        raise AssertionError("no VIP routes b.test.local: /api→b, /→a")


    def whoami_manifest(name, greeting, port):
        return (
            "apiVersion: expanse.io/v1\nkind: Block\n"
            f"metadata:\n  name: {name}\n  namespace: default\n"
            "spec:\n  type: web/whoami\n  replicas: 1\n"
            "  resources:\n    requests:\n      cpu: 100m\n      memory: 64Mi\n"
            f"  config:\n    port: {port}\n    greeting: \"{greeting}\"\n"
            "  network:\n    ports:\n"
            f"      - name: http\n        port: 80\n        target_port: {port}\n"
            "        protocol: tcp\n        expose: EXPOSE_VIP\n"
            "    health_check:\n      readiness:\n        type: PROBE_TCP\n"
            f"        port: {port}\n        period_seconds: 2\n"
        )

    with subtest("deploy edge (route declarations), then upstreams a and b"):
        edge = (
            whoami_manifest("edge", "edge-", 8080).replace(
                "        protocol: tcp\n        expose: EXPOSE_VIP\n",
                "        protocol: tcp\n        expose: EXPOSE_VIP\n"
                "        http_routes:\n"
                "          - host: a.test.local\n            path_prefix: /api\n"
                "            service: a\n"
                "          - host: a.test.local\n            path_prefix: /\n"
                "            service: a\n"
                "          - host: b.test.local\n            path_prefix: /api\n"
                "            service: b\n"
                "          - host: b.test.local\n            path_prefix: /\n"
                "            service: a\n",
            )
        )
        deploy(n1, "edge", edge)
        deploy(n1, "a", whoami_manifest("a", "a-", 8081))
        deploy(n1, "b", whoami_manifest("b", "b-", 8082))

    with subtest("all three blocks Running within 90 s"):
        for name in ("edge", "a", "b"):
            b = wait_phase(n1, name, ["RUNNING"], 90)
            assert len(placement_nodes(b)) == 1, f"{name}: {b.get('status')}"

    with subtest("edge VIP serves L7"):
        vip = find_edge_vip()
        print("edge VIP:", vip)
        deadline = time.time() + 60
        out = ""
        while time.time() < deadline:
            rc, out = n9.execute(
                f"curl -s --connect-timeout 3 -H 'Host: a.test.local' http://{vip}/ || true")
            if "a-replica-0" in out:
                break
            time.sleep(2)
        assert "a-replica-0" in out, f"edge VIP never served: {out!r}"

    def curl(host, path):
        rc, out = n9.execute(
            f"curl -s --connect-timeout 3 --max-time 5 -H 'Host: {host}' "
            f"http://{vip}{path} || true")
        return out

    with subtest("Host-based routing (G5.8)"):
        out = curl("a.test.local", "/")
        assert "a-replica-0" in out, f"a.test.local/ → {out!r}"
        out = curl("a.test.local", "/api/thing")
        assert "a-replica-0" in out, f"a.test.local/api/thing → {out!r}"

    with subtest("path-prefix routing: /api → block b, / → block a"):
        out = curl("b.test.local", "/api")
        assert "b-replica-0" in out, f"b.test.local/api → {out!r}"
        out = curl("b.test.local", "/api/more")
        assert "b-replica-0" in out, f"b.test.local/api/more → {out!r}"
        out = curl("b.test.local", "/")
        assert "a-replica-0" in out, f"b.test.local/ → {out!r}"
        # Segment boundary: /apiv2 must NOT match /api.
        out = curl("b.test.local", "/apiv2")
        assert "a-replica-0" in out, f"b.test.local/apiv2 → {out!r}"

    with subtest("unknown host → 404"):
        out = curl("nope.test.local", "/")
        assert out.strip() in ("no route", ""), f"unknown host → {out!r}"
        rc, code = n9.execute(
            "curl -s -o /dev/null -w '%{http_code}' --connect-timeout 3 "
            f"-H 'Host: nope.test.local' http://{vip}/ || true")
        assert code.strip() == "404", f"unknown host status = {code!r}"

    with subtest("X-Forwarded-For carries the real client IP (G5.8)"):
        rc, cip = n9.execute(
            "ip -4 -o addr show eth1 | grep -oE '192\\.168\\.1\\.[0-9]+' | head -1")
        cip = cip.strip()
        assert cip, "could not determine client eth1 IP"
        rc, hdrs = n9.execute(
            f"curl -s -D - -o /dev/null -H 'Host: a.test.local' http://{vip}/ || true")
        assert f"X-Seen-Xff: {cip}" in hdrs, f"headers: {hdrs!r}"
  '';
}
