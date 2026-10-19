"""node-stock-blocks (1.11.1): a block on a node as an install leaves it, with nothing pre-opened.

The NixOS firewall is on with no extra ports, and web/nginx's runtime is not in the store, so the
agent builds it itself under the real expansed unit. n9, outside the cluster, reaches the block on
its LAN address. Runs after cluster-common.py, block-common.py, vol_cluster.py and
single_node_common.py.
"""

SITE_YAML = (
    "apiVersion: expanse.io/v1\nkind: Block\n"
    "metadata:\n  name: site\n  namespace: default\n"
    "spec:\n  type: web/nginx\n  replicas: 1\n"
    "  resources:\n    requests:\n      cpu: 100m\n      memory: 64Mi\n"
    "  config:\n    port: 8080\n    serverName: site\n"
    "  network:\n    ports:\n"
    "      - name: http\n        port: 80\n        target_port: 8080\n"
    "        protocol: tcp\n        expose: EXPOSE_VIP\n"
    "    health_check:\n      readiness:\n        type: PROBE_TCP\n"
    "        port: 8080\n        period_seconds: 2\n"
)
RUNTIME_LINK = "/nix/var/nix/gcroots/expanse/block-runtime/web-nginx"


def block_vip():
    out = n1.execute("expanse ctl kv --socket /run/expanse/agent.sock get /network/vipPool/external/default/site 2>&1")[1]
    try:
        return json.loads(out)["addr"].split("/")[0]
    except (ValueError, KeyError):
        return ""


def vip_serves():
    vip = block_vip()
    out = n9.execute(f"curl -s -o /dev/null -w '%{{http_code}}' --connect-timeout 3 http://{vip}/ || true")[1]
    return vip != "" and out.strip() == "200"


def runtime_built():
    return n1.execute(f"test -x {RUNTIME_LINK}/bin/nginx")[0] == 0


form_single("stock")
n9.start()

with subtest("nginx's runtime is not in the store before the first placement"):
    out = n1.succeed("cat /etc/expanse/block-runtimes.json")
    n1.fail(f"test -e {json.loads(out)['web/nginx']}")

with subtest("the agent builds the runtime itself and nginx runs"):
    deploy(n1, "site", SITE_YAML)
    try:
        wait_for(runtime_built, "the agent to build web/nginx's runtime", 300)
    finally:
        print(n1.execute("journalctl -u expansed --no-pager | grep -i 'runtime build' | tail -5")[1])
    wait_phase(n1, "site", ["RUNNING"], 120)

with subtest("a client outside the cluster reaches the block on its LAN address"):
    wait_for(vip_serves, "n9 to reach nginx on the block's LAN address", 90)

print("NODE-STOCK-BLOCKS PASSED: runtime built by the agent, block served through the stock firewall")
