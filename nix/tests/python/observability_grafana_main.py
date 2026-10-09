"""observability-grafana: PHASE-09-TASKS.md Stream C (X3). The shipped
Grafana provisioning (deploy/grafana/provisioning/) and dashboard
(deploy/grafana/dashboards/expanse-cluster-health.json) are loaded by a
real Grafana server pointed at a real Prometheus (itself scraping a real
3-node cluster, Stream A's own setup) -- and the dashboard's own panel
queries are proven to return real data back through Grafana's own HTTP
API (/api/ds/query), not just "the dashboard JSON exists".

Runs after cluster-common.py; NODES/n1/n2/n3/form/wait_quorum/
wait_agent_ready come from that shared file. GRAFANA_HOMEPATH/
DATASOURCE_YAML/DASHBOARDS_YAML/DASHBOARD_JSON are injected by
observability-grafana.nix (real nix store paths).
"""

import json

SOCK = "/run/expanse/agent.sock"
TOKEN = "vm-test-grafana-token-do-not-log-me"
CAFILE = "/root/expanse-ca.pem"
PROM = "http://127.0.0.1:9090"
GRAFANA = "http://127.0.0.1:3000"
DS_UID = "expanse-prometheus"


def apply_file(m, rid, path, content):
    rc, out = m.execute(
        "expanse ctl resource apply --socket " + SOCK + " -f - <<'EOF'\n"
        f"{rid}:\n  type: file\n  path: {path}\n  content: {content}\n  mode: \"0644\"\nEOF\n"
    )
    assert rc == 0, f"{m.name} apply failed: {out}"


def prom_query(m, promql):
    """Runs a real PromQL query against the real running Prometheus and
    returns the decoded `data.result` list -- empty if no series matched."""
    out = m.succeed(f"curl -sf '{PROM}/api/v1/query' --data-urlencode 'query={promql}'")
    doc = json.loads(out)
    assert doc["status"] == "success", f"query {promql!r} failed: {doc}"
    return doc["data"]["result"]


def grafana_get(m, path):
    out = m.succeed(f"curl -sf -u admin:admin '{GRAFANA}{path}'")
    return json.loads(out)


def grafana_ds_query(m, expr):
    """Runs expr through Grafana's own /api/ds/query -- the real backend
    query path a dashboard panel uses to render, not a raw Prometheus
    query -- against the provisioned Expanse Prometheus datasource, and
    returns the frame's own value series (the last values[] array; the
    first is always the paired timestamp array)."""
    body = json.dumps(
        {
            "queries": [
                {
                    "refId": "A",
                    "datasource": {"type": "prometheus", "uid": DS_UID},
                    "expr": expr,
                    "instant": True,
                    "range": False,
                }
            ],
            "from": "now-5m",
            "to": "now",
        }
    )
    out = m.succeed(
        f"curl -sf -u admin:admin -H 'Content-Type: application/json' "
        f"-d '{body}' {GRAFANA}/api/ds/query"
    )
    doc = json.loads(out)
    frame = doc["results"]["A"]["frames"][0]
    return frame["data"]["values"][-1]


form("test")

with subtest("real desired state: a healthy file resource on n1"):
    apply_file(n1, "file:/etc/grafana-probe", "/etc/grafana-probe", "alpha")
    n1.wait_until_succeeds("grep -qx alpha /etc/grafana-probe", timeout=60)

with subtest("a real replicated volume on n1"):
    n1.succeed("expanse ctl volume create gvol --size 64Mi --replication 3")

with subtest("set a known metrics token and a world-readable CA copy"):
    n1.succeed(f"expanse ctl metrics set-token --token '{TOKEN}'")
    n1.succeed("install -m0644 /persist/expanse/ca/ca.pem " + CAFILE)
    n1.succeed(f"printf '%s' '{TOKEN}' > /root/metrics-token && chmod 0644 /root/metrics-token")

with subtest("a real prometheus scrapes n1's /metrics over mTLS with the bearer token"):
    n1.succeed(
        "cat > /root/prometheus.yml <<'EOF'\n"
        "scrape_configs:\n"
        "  - job_name: expanse\n"
        "    scheme: https\n"
        "    scrape_interval: 2s\n"
        "    tls_config:\n"
        f"      ca_file: {CAFILE}\n"
        "      server_name: n1\n"
        "    bearer_token_file: /root/metrics-token\n"
        "    static_configs:\n"
        "      - targets: ['127.0.0.1:7447']\n"
        "EOF\n"
    )
    n1.execute(
        "nohup prometheus --config.file=/root/prometheus.yml "
        "--storage.tsdb.path=/root/promdata "
        "--web.listen-address=127.0.0.1:9090 > /root/prom.log 2>&1 &"
    )
    n1.wait_for_open_port(9090)
    n1.wait_until_succeeds(
        f"curl -sf {PROM}/api/v1/targets | grep -q '\"health\":\"up\"'", timeout=60
    )

with subtest("the volume reaches Healthy before Grafana ever looks at it"):
    n1.wait_until_succeeds(
        f"curl -sf '{PROM}/api/v1/query' --data-urlencode 'query=expanse_volume_health{{volume=\"gvol\"}}' "
        "| grep -q '\"2\"'",
        timeout=60,
    )

with subtest("install the shipped Grafana provisioning and dashboard, unmodified"):
    n1.succeed("mkdir -p /root/grafana/provisioning/datasources /root/grafana/provisioning/dashboards")
    n1.succeed(f"install -m0644 {DATASOURCE_YAML} /root/grafana/provisioning/datasources/datasource.yaml")
    n1.succeed(f"install -m0644 {DASHBOARDS_YAML} /root/grafana/provisioning/dashboards/dashboards.yaml")
    # dashboards.yaml's own shipped path, verbatim -- not adjusted by the test.
    n1.succeed("mkdir -p /etc/grafana/dashboards/expanse")
    n1.succeed(f"install -m0644 {DASHBOARD_JSON} /etc/grafana/dashboards/expanse/expanse-cluster-health.json")

with subtest("a real grafana loads the shipped provisioning and dashboard"):
    n1.succeed(
        "cat > /root/grafana.ini <<'EOF'\n"
        "[paths]\n"
        "data = /root/grafana-data\n"
        "logs = /root/grafana-data/log\n"
        "plugins = /run/current-system/sw/lib/grafana/plugins\n"
        "provisioning = /root/grafana/provisioning\n"
        "[server]\n"
        "http_addr = 127.0.0.1\n"
        "http_port = 3000\n"
        "[security]\n"
        "admin_user = admin\n"
        "admin_password = admin\n"
        "[auth.anonymous]\n"
        "enabled = false\n"
        # This VM has no network route out -- without these, Grafana's
        # background plugin preinstaller and update checker retry
        # unreachable grafana.com calls for tens of seconds, starving the
        # embedded sqlite of write locks and stalling every other
        # provisioning step behind it.
        "[analytics]\n"
        "reporting_enabled = false\n"
        "check_for_updates = false\n"
        "[plugins]\n"
        "preinstall_disabled = true\n"
        "EOF\n"
    )
    n1.execute(
        f"nohup grafana server --config=/root/grafana.ini --homepath={GRAFANA_HOMEPATH} "
        "> /root/grafana.log 2>&1 &"
    )
    n1.wait_for_open_port(3000)
    # Unlike Prometheus's compact JSON, Grafana's API responses are
    # pretty-printed -- a space after the colon -- so the match pattern
    # has to allow for it.
    n1.wait_until_succeeds(f"curl -sf {GRAFANA}/api/health | grep -q '\"database\": \"ok\"'", timeout=60)

with subtest("the datasource really provisioned -- not just the yaml exists"):
    datasources = grafana_get(n1, "/api/datasources")
    uids = {d["uid"]: d for d in datasources}
    assert DS_UID in uids, f"expanse-prometheus datasource not provisioned: {uids}"
    assert uids[DS_UID]["type"] == "prometheus", uids[DS_UID]

with subtest("the dashboard really provisioned -- not just the json exists"):
    n1.wait_until_succeeds(
        f"curl -sf -u admin:admin '{GRAFANA}/api/dashboards/uid/expanse-health' | grep -q 'Node Health'",
        timeout=60,
    )
    doc = grafana_get(n1, "/api/dashboards/uid/expanse-health")
    titles = {p["title"] for p in doc["dashboard"]["panels"]}
    assert {"Node Health", "Resource Health", "Volume Health", "Quorum"} <= titles, titles

with subtest("Node Health panel query returns real data through Grafana's own API"):
    values = grafana_ds_query(n1, "expanse_node_health")
    assert values, "expanse_node_health returned no data via /api/ds/query"

with subtest("Resource Health panel query reflects the real healthy file resource"):
    values = grafana_ds_query(n1, 'expanse_resource_health{id="file:/etc/grafana-probe"}')
    assert values and values[0] == 1, f"resource health via grafana = {values}, want healthy (1)"

with subtest("Volume Health panel query reflects the real healthy volume"):
    values = grafana_ds_query(n1, 'expanse_volume_health{volume="gvol"}')
    assert values and values[0] == 2, f"volume health via grafana = {values}, want healthy (2)"

with subtest("Quorum panel query reflects the real 3-node cluster"):
    values = grafana_ds_query(n1, "expanse_quorum_voters")
    assert values and values[0] == 3, f"quorum voters via grafana = {values}"
    values = grafana_ds_query(n1, "expanse_quorum_has_leader")
    assert values and values[0] == 1, f"quorum has_leader via grafana = {values}"

print("OBSERVABILITY-GRAFANA DONE")
