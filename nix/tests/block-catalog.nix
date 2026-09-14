# §8 block-catalog: deploy all 6 shipped blocks and assert each is
# functional (G4.14): nginx HTTP 200, `redis-cli ping`, ollama
# /api/tags, node-exporter /metrics, static-site index, echo body.
# Each shipped type runs through the full pipeline: catalog validation
# (V3/V19 against nix/blocks/<cat>/<name>/schema.json) → placement →
# bridge → reconciler → expanse-block-run workload (workloads.go —
# binary-backed types exec the upstream binaries from PATH).
{ self }:
{ pkgs, lib, ... }:
let
  expanse = self.packages.${pkgs.system}.expanse;
in
{
  name = "expanse-block-catalog";

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
      environment.systemPackages = with pkgs; [
        openssl curl jq redis nginx ollama prometheus-node-exporter
      ];
      virtualisation.memorySize = 4096;
      virtualisation.diskSize = 8192;
      expanse.agent.period = "5s";
      expanse.agent.controllerPeriod = "5s";
      expanse.agent.blocksCatalog = ../blocks;
      expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
      environment.etc."expanse/blocks-flake".source = ../blocks-flake;
      networking.firewall.allowedTCPPortRanges = [ { from = 18000; to = 18999; } ];
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
      environment.systemPackages = with pkgs; [
        openssl curl nginx ollama prometheus-node-exporter
      ];
      virtualisation.memorySize = 4096;
      virtualisation.diskSize = 8192;
      expanse.agent.period = "5s";
      expanse.agent.controllerPeriod = "5s";
      expanse.agent.blocksCatalog = ../blocks;
      expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
      environment.etc."expanse/blocks-flake".source = ../blocks-flake;
      networking.firewall.allowedTCPPortRanges = [ { from = 18000; to = 18999; } ];
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
      environment.systemPackages = with pkgs; [
        openssl curl nginx ollama prometheus-node-exporter
      ];
      virtualisation.memorySize = 4096;
      virtualisation.diskSize = 8192;
      expanse.agent.period = "5s";
      expanse.agent.controllerPeriod = "5s";
      expanse.agent.blocksCatalog = ../blocks;
      expanse.agent.blocksFlakeRef = "/etc/expanse/blocks-flake";
      environment.etc."expanse/blocks-flake".source = ../blocks-flake;
      networking.firewall.allowedTCPPortRanges = [ { from = 18000; to = 18999; } ];
    };
  };

  testScript = ''
    ${builtins.readFile ./cluster-common.py}
    ${builtins.readFile ./block-common.py}

    def deploy_type(m, name, ytype, config, replicas=1, strategy=None):
        """Deploy an arbitrary shipped block type with the given config."""
        reps = f"  replicas: {replicas}\n" if replicas is not None else ""
        st = f"  strategy:\n    kind: {strategy}\n" if strategy else ""
        cfg = "\n".join(f"    {k}: {v}" for k, v in config.items()) + "\n"
        y = (
            "apiVersion: expanse.io/v1\nkind: Block\n"
            f"metadata:\n  name: {name}\n  namespace: default\n"
            f"spec:\n  type: {ytype}\n"
            f"{reps}{st}"
            "  resources:\n    requests:\n      cpu: 100m\n      memory: 64Mi\n"
            "  config:\n"
            f"{cfg}"
        )
        deploy(m, name, y)

    form("test")

    with subtest("deploy all 6 shipped blocks"):
        deploy_type(n1, "echo", "util/echo",
                    {"port": 18100, "body": '"catalog-echo\\n"'})
        deploy_type(n1, "nginx", "web/nginx",
                    {"port": 18101, "serverName": "cat.example.internal"})
        deploy_type(n1, "redis", "db/redis",
                    {"port": 18102, "maxMemory": "64mb", "maxMemoryPolicy": "noeviction"})
        # node-exporter ships as a daemonset (one per node).
        deploy_type(n1, "nodeexp", "monitor/node-exporter",
                    {"port": 18103}, replicas=None, strategy="DAEMONSET")
        deploy_type(n1, "static", "web/static-site",
                    {"port": 18104, "index": "<html><body>catalog-static</body></html>"})
        deploy_type(n1, "ollama", "ai/ollama", {"port": 18105})

    with subtest("all five single-replica blocks RUNNING within 60 s"):
        for name in ["echo", "nginx", "redis", "static", "ollama"]:
            b = wait_phase(n1, name, ["RUNNING"], 60)
        b = wait_phase(n1, "nodeexp", ["RUNNING"], 60)
        assert len(placement_nodes(b)) == 3, f"daemonset not on 3 nodes: {b.get('status')}"

    def node_ip(name, idx=0):
        b = get_json(n1, name)
        node = replica_node(b, idx)
        assert node is not None, f"{name} has no live placement: {b.get('status')}"
        return IP[node]

    def http_has(m, url, needle, code="200"):
        rc, out = m.execute(f"curl -sS --max-time 5 -w '\\n%{{http_code}}' {url}")
        assert out.strip().endswith(code), f"{url}: want HTTP {code}, got {out[-60:]!r}"
        assert needle in out, f"{url}: {needle!r} not in response: {out[:120]!r}"

    with subtest("each block is functional"):
        # util/echo
        http_has(n1, f"http://{node_ip('echo')}:18100/", "catalog-echo")
        # web/nginx — HTTP 200 from the real nginx
        http_has(n1, f"http://{node_ip('nginx')}:18101/", "expanse nginx block")
        # db/redis — real redis-cli ping (protected mode keeps remote
        # clients out; ping from the hosting node)
        machines_by_host = {"n1": n1, "n2": n2, "n3": n3}
        rhost = node_ip("redis")
        rc, out = machines_by_host[{"192.168.1.1": "n1", "192.168.1.2": "n2", "192.168.1.3": "n3"}[rhost]].execute(
            "redis-cli -p 18102 ping")
        assert "PONG" in out, f"redis ping: {out!r}"
        # monitor/node-exporter — metrics from every node
        for m in [n1, n2, n3]:
            ip = addr(m)
            rc, out = m.execute(f"curl -sS --max-time 5 http://{ip}:18103/metrics")
            assert "node_uname_info" in out, f"no metrics from {m.name}: {out[:80]!r}"
        # web/static-site
        http_has(n1, f"http://{node_ip('static')}:18104/", "catalog-static")
        # ai/ollama — the real ollama serves /api/tags
        http_has(n1, f"http://{node_ip('ollama')}:18105/api/tags", "models")

    with subtest("block list shows all 6"):
        rc, out = n1.execute(f"expanse ctl block list {SOCK} -o json")
        names = [b.get("metadata", {}).get("name")
                 for b in json.loads(out[out.find("["):out.rfind("]") + 1] or "[]")]
        for want in ["echo", "nginx", "redis", "nodeexp", "static", "ollama"]:
            assert want in names, f"{want} missing from list: {names}"
  '';
}
