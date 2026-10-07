"""monitor/prometheus + monitor/grafana: the cluster observes itself.

A node-exporter daemonset, a SINGLETON Prometheus on a replicated volume
(handed every node and the cluster CA by the bridge) and a Grafana pointed
at it. Proves every agent's /metrics and every node-exporter are scraped
over the real auth, the shipped rules load, and the shipped dashboard's
queries return data through Grafana.

Runs after cluster-common.py and block-common.py.
"""

TOKEN = "vm-test-monitoring-token"
PROM_PORT = 18110
GRAFANA_PORT = 18111
NODEEXP_PORT = 18112
ADMIN_PW = "monitoring-admin"
HOSTS = {"n1": n1, "n2": n2, "n3": n3}


def block_yaml(name, typ, config, storage=True, strategy="SINGLETON"):
    cfg = "".join(f"    {k}: {v}\n" for k, v in config.items())
    st = (
        "  storage:\n"
        f"    - name: {name}-data\n      size: 128Mi\n      replication: 3\n"
        f"      mountPath: /var/lib/{name}\n"
    ) if storage else ""
    reps = "" if strategy == "DAEMONSET" else "  replicas: 1\n"
    return (
        "apiVersion: expanse.io/v1\nkind: Block\n"
        f"metadata:\n  name: {name}\n  namespace: default\n"
        f"spec:\n  type: {typ}\n{reps}"
        f"  strategy:\n    kind: {strategy}\n"
        "  resources:\n    requests:\n      cpu: 100m\n      memory: 128Mi\n"
        f"{st}  config:\n{cfg}"
    )


def host_of(name):
    """The node id and address currently running replica 0 of name."""
    node = replica_node(get_json(n1, name), 0)
    assert node is not None, f"{name} has no live placement"
    return node, addr(HOSTS[node])


def prom_json(path):
    doc = prom_try(path)
    assert doc is not None, f"prometheus {path} unreachable"
    return doc


def prom_try(path):
    """Decoded Prometheus API response, or None while it is down."""
    _, ip = host_of("prom")
    rc, out = n1.execute(f"curl -sf 'http://{ip}:{PROM_PORT}{path}'")
    return json.loads(out) if rc == 0 else None


def targets_up():
    """job/node -> health for every active scrape target; empty while down."""
    doc = prom_try("/api/v1/targets?state=active")
    if doc is None:
        return {}
    return {
        f"{t['labels']['job']}/{t['labels'].get('node', '')}": t["health"]
        for t in doc["data"]["activeTargets"]
    }


form("monitoring")
for m in HOSTS.values():
    wait_agent_ready(m)

with subtest("set a known metrics token"):
    n1.succeed(f"expanse ctl metrics set-token --token '{TOKEN}'")

with subtest("node-exporter daemonset, Prometheus and Grafana deploy"):
    deploy(n1, "nodeexp", block_yaml("nodeexp", "monitor/node-exporter",
                                     {"port": NODEEXP_PORT}, storage=False, strategy="DAEMONSET"))
    deploy(n1, "prom", block_yaml("prom", "monitor/prometheus", {
        "port": PROM_PORT, "metricsToken": TOKEN, "scrapeInterval": "2s",
        "nodeExporterPort": NODEEXP_PORT,
    }))
    wait_phase(n1, "nodeexp", ["RUNNING"], 120)
    wait_phase(n1, "prom", ["RUNNING"], 180)
    # RUNNING means the unit is active, not that Prometheus is serving yet.
    _, prom_ip = host_of("prom")
    n1.wait_until_succeeds(f"curl -sf http://{prom_ip}:{PROM_PORT}/-/ready", timeout=120)

with subtest("every agent and every node-exporter is scraped and up"):
    want = {f"expanse-{n}/{n}" for n in HOSTS} | {f"node-exporter/{n}" for n in HOSTS} | {"prometheus/"}
    deadline = time.time() + 90
    got = {}
    while time.time() < deadline:
        got = targets_up()
        if want <= set(got) and all(got[k] == "up" for k in want):
            break
        time.sleep(3)
    else:
        raise AssertionError(f"targets never all up: want {sorted(want)}, got {got}")

with subtest("the agents' own series arrive: 3 voters with a leader"):
    def voters():
        res = prom_json("/api/v1/query?query=max(expanse_quorum_voters)")["data"]["result"]
        return res[0]["value"][1] if res else None
    deadline = time.time() + 60
    while voters() != "3" and time.time() < deadline:
        time.sleep(2)
    assert voters() == "3", f"expanse_quorum_voters = {voters()}"

with subtest("the shipped alert rules are loaded and evaluating"):
    groups = prom_json("/api/v1/rules")["data"]["groups"]
    rules = {r["name"] for g in groups for r in g["rules"]}
    assert {"ExpanseNodeUnhealthy", "ExpanseNodeUnreachable", "ExpanseQuorumNoLeader"} <= rules, rules

with subtest("Grafana deploys pointed at the Prometheus block"):
    _, prom_ip = host_of("prom")
    deploy(n1, "grafana", block_yaml("grafana", "monitor/grafana", {
        "port": GRAFANA_PORT, "prometheusUrl": f"http://{prom_ip}:{PROM_PORT}",
        "adminPassword": ADMIN_PW,
    }))
    wait_phase(n1, "grafana", ["RUNNING"], 180)
    _, g_ip = host_of("grafana")
    GRAFANA = f"http://{g_ip}:{GRAFANA_PORT}"
    n1.wait_until_succeeds(f"curl -sf {GRAFANA}/api/health | grep -q '\"database\": \"ok\"'", timeout=120)

with subtest("the shipped dashboard is provisioned"):
    n1.wait_until_succeeds(
        f"curl -sf -u admin:{ADMIN_PW} {GRAFANA}/api/dashboards/uid/expanse-health | grep -q 'Node Health'",
        timeout=60,
    )

with subtest("a dashboard query returns real data through Grafana"):
    body = json.dumps({
        "queries": [{"refId": "A", "datasource": {"type": "prometheus", "uid": "expanse-prometheus"},
                     "expr": "max(expanse_quorum_voters)", "instant": True, "range": False}],
        "from": "now-5m", "to": "now",
    })
    out = n1.succeed(
        f"curl -sf -u admin:{ADMIN_PW} -H 'Content-Type: application/json' -d '{body}' {GRAFANA}/api/ds/query"
    )
    values = json.loads(out)["results"]["A"]["frames"][0]["data"]["values"][-1]
    assert values and values[0] == 3, f"voters via grafana = {values}"


def oldest_sample():
    """Earliest timestamp Prometheus holds for its own up series; None while down."""
    doc = prom_try("/api/v1/query?query=up%7Bjob%3D%22prometheus%22%7D%5B1h%5D")
    res = doc["data"]["result"] if doc else []
    return min(float(v[0]) for v in res[0]["values"]) if res else None


with subtest("Prometheus keeps its TSDB across a restart on the volume"):
    before = oldest_sample()
    assert before is not None, "no self-scrape history before the restart"
    node, _ = host_of("prom")
    HOSTS[node].succeed("systemctl restart expanse-block@default-prom-0.service")
    after = None
    deadline = time.time() + 90
    while time.time() < deadline:
        after = oldest_sample()
        if after is not None:
            break
        time.sleep(3)
    assert after is not None and after <= before, f"history lost on restart: oldest {before} -> {after}"
