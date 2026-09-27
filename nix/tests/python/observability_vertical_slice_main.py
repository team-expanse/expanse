"""observability-vertical-slice: PHASE-09-TASKS.md Stream E (X5, the
release blocker). ROADMAP.md's own exit line for this phase, verbatim:
"a node failure raises an alert and is visible in the UI and dashboards
within 30s" -- measured end to end with a real killed node
(Machine.crash(), the project's own established "hard-killed" idiom,
see db_postgres_failover.py/vm_instance_failover.py/
iscsi_vertical_slice.py) and a real stopwatch, not assumed from each
component's own latency budget (D6).

A lone follower dying in a healthy 3-node Raft cluster loses neither
quorum (2/3 remain) nor produces a self-report (a dead node can't
report anything) -- so the only signal that can possibly fire is
nodelc's leader-side failure monitor (sec 4.8, 15s silent ->
unreachable), which this stream wired all the way through:
internal/cluster/control.NodeStatus.Lifecycle ->
internal/metrics's new expanse_quorum_node_unreachable gauge ->
deploy/prometheus/expanse-alerts.rules.yml's new ExpanseNodeUnreachable
critical rule -> internal/web/health.go's buildAlerts -> this
dashboard's new "Per-node unreachable flag" panel. Every hop reuses the
prior streams' own already-proven mechanism (Stream A's scrape,
Stream B's rule-firing proof, Stream C's Grafana provisioning,
Stream D's SSE-pushed /health page) -- this stream adds one new signal
and then measures the whole assembled pipeline for real.

Runs after cluster-common.py; NODES/n1/n2/n3/form/wait_quorum/
wait_agent_ready come from that shared file. RULES_FILE/GRAFANA_HOMEPATH/
DATASOURCE_YAML/DASHBOARDS_YAML/DASHBOARD_JSON are injected by
observability-vertical-slice.nix (real nix store paths).
"""

import json

TOKEN = "vm-test-vslice-token-do-not-log-me"
CAFILE = "/root/expanse-ca.pem"
UI_CAFILE = "/persist/expanse/ca/ui-ca.pem"  # the web UI's own CA (ECDSA; browsers reject the cluster CA's Ed25519)
PROM = "http://127.0.0.1:9090"
GRAFANA = "http://127.0.0.1:3000"
DS_UID = "expanse-prometheus"
UI_PASSWORD = "vslice-password"
# 2s/2s (not D6's originally-recommended 10-15s): nodelc's own 15-20s
# detection latency (unreachable threshold plus its 5s monitor tick,
# sec 4.8) already spends most of the 30s budget, so this stream
# revises D6 down -- see PHASE-09-TASKS.md's D6 entry for the real
# numbers this test itself measures.
SCRAPE_INTERVAL = "2s"
# The 30s budget itself, exactly as ROADMAP.md states it. The poll
# timeouts below are longer, purely to absorb VM scheduling jitter
# without a flaky hard failure -- the actual pass/fail check on the
# 30s budget is the explicit assert against each measured elapsed time.
BUDGET_SECONDS = 30


def ui_login(m, password=UI_PASSWORD, jar="/root/ui-cookies.txt"):
    """Log in against m's own UI listener; returns the CSRF token."""
    m.succeed(f"rm -f {jar}")
    code = m.succeed(
        f"curl -s -o /dev/null -w '%{{http_code}}' -c {jar} "
        f"--resolve {m.name}:8443:127.0.0.1 --cacert {UI_CAFILE} "
        f"-d 'username=admin&password={password}' https://{m.name}:8443/login"
    ).strip()
    assert code == "303", f"login on {m.name} returned {code}, want 303"
    csrf = m.succeed(f"awk -F'\\t' '$6==\"expanse_csrf\"{{print $7}}' {jar}").strip()
    assert csrf, f"no expanse_csrf cookie in {jar} on {m.name}"
    return csrf


def prom_query(m, promql):
    out = m.succeed(f"curl -sf '{PROM}/api/v1/query' --data-urlencode 'query={promql}'")
    doc = json.loads(out)
    assert doc["status"] == "success", f"query {promql!r} failed: {doc}"
    return doc["data"]["result"]


def grafana_get(m, path):
    return json.loads(m.succeed(f"curl -sf -u admin:admin '{GRAFANA}{path}'"))


def grafana_ds_query(m, expr):
    """Real backend query path a dashboard panel uses to render (not a
    raw Prometheus query) -- see observability_grafana_main.py, whose
    own helper this mirrors verbatim."""
    body = json.dumps(
        {
            "queries": [{"refId": "A", "datasource": {"type": "prometheus", "uid": DS_UID}, "expr": expr, "instant": True, "range": False}],
            "from": "now-5m",
            "to": "now",
        }
    )
    out = m.succeed(f"curl -sf -u admin:admin -H 'Content-Type: application/json' -d '{body}' {GRAFANA}/api/ds/query")
    doc = json.loads(out)
    frame = doc["results"]["A"]["frames"][0]
    return frame["data"]["values"][-1]


def poll_elapsed(t0, check, timeout, desc):
    """Polls check() until truthy, returns the real elapsed time since
    t0 -- the measurement this whole test exists to produce."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        if check():
            return time.time() - t0
        time.sleep(0.5)
    raise AssertionError(f"{desc}: not observed within {timeout}s of the kill")


form("vslice")
for m in [n1, n2, n3]:
    wait_agent_ready(m)

with subtest("set a known admin password, metrics token and a world-readable CA copy"):
    n1.succeed(f"expanse ctl admin reset-password --password '{UI_PASSWORD}'")
    n1.succeed(f"expanse ctl metrics set-token --token '{TOKEN}'")
    n1.succeed("install -m0644 /persist/expanse/ca/ca.pem " + CAFILE)
    n1.succeed(f"printf '%s' '{TOKEN}' > /root/metrics-token && chmod 0644 /root/metrics-token")
    n1.succeed(f"install -m0644 {RULES_FILE} /root/expanse-alerts.rules.yml")

with subtest("a real prometheus loads the (Stream E-extended) rule file and scrapes n1"):
    n1.succeed(
        "cat > /root/prometheus.yml <<'EOF'\n"
        "global:\n"
        f"  scrape_interval: {SCRAPE_INTERVAL}\n"
        f"  evaluation_interval: {SCRAPE_INTERVAL}\n"
        "rule_files:\n"
        "  - /root/expanse-alerts.rules.yml\n"
        "scrape_configs:\n"
        "  - job_name: expanse\n"
        "    scheme: https\n"
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
    n1.wait_until_succeeds(f"curl -sf {PROM}/api/v1/targets | grep -q '\"health\":\"up\"'", timeout=60)
    n1.wait_until_succeeds(f"curl -sf {PROM}/api/v1/rules | grep -q ExpanseNodeUnreachable", timeout=60)

with subtest("install and start a real grafana on the shipped provisioning, unmodified"):
    n1.succeed("mkdir -p /root/grafana/provisioning/datasources /root/grafana/provisioning/dashboards")
    n1.succeed(f"install -m0644 {DATASOURCE_YAML} /root/grafana/provisioning/datasources/datasource.yaml")
    n1.succeed(f"install -m0644 {DASHBOARDS_YAML} /root/grafana/provisioning/dashboards/dashboards.yaml")
    n1.succeed("mkdir -p /etc/grafana/dashboards/expanse")
    n1.succeed(f"install -m0644 {DASHBOARD_JSON} /etc/grafana/dashboards/expanse/expanse-cluster-health.json")
    n1.succeed(
        "cat > /root/grafana.ini <<'EOF'\n"
        "[paths]\n"
        "data = /root/grafana-data\n"
        "logs = /root/grafana-data/log\n"
        "plugins = /root/grafana-data/plugins\n"
        "provisioning = /root/grafana/provisioning\n"
        "[server]\n"
        "http_addr = 127.0.0.1\n"
        "http_port = 3000\n"
        "[security]\n"
        "admin_user = admin\n"
        "admin_password = admin\n"
        "[auth.anonymous]\n"
        "enabled = false\n"
        "[analytics]\n"
        "reporting_enabled = false\n"
        "check_for_updates = false\n"
        "[plugins]\n"
        "preinstall_disabled = true\n"
        "EOF\n"
    )
    n1.execute(f"nohup grafana server --config=/root/grafana.ini --homepath={GRAFANA_HOMEPATH} > /root/grafana.log 2>&1 &")
    n1.wait_for_open_port(3000)
    n1.wait_until_succeeds(f"curl -sf {GRAFANA}/api/health | grep -q '\"database\": \"ok\"'", timeout=60)
    n1.wait_until_succeeds(f"curl -sf -u admin:admin '{GRAFANA}/api/dashboards/uid/expanse-health' | grep -q 'Per-node unreachable flag'", timeout=60)

with subtest("log in to n1's UI and open the live /health SSE stream in the background"):
    csrf = ui_login(n1)
    n1.execute(
        f"nohup curl -s -N -b /root/ui-cookies.txt --resolve n1:8443:127.0.0.1 "
        f"--cacert {UI_CAFILE} https://n1:8443/health/events "
        "> /root/health-sse.log 2>&1 & echo started"
    )
    time.sleep(3)  # let the watch land before the kill (Stream D's own watch-before-snapshot ordering)
    baseline = n1.succeed("cat /root/health-sse.log")
    assert "ExpanseNodeUnreachable" not in baseline, f"alert already present before the kill: {baseline}"

with subtest("baseline: n3 is healthy and not yet unreachable anywhere in the pipeline"):
    result = prom_query(n1, 'expanse_quorum_node_unreachable{node="n3"}')
    assert result and result[0]["value"][1] == "0", f"n3 already unreachable before the kill: {result}"
    body = n1.succeed(f"curl -sf -b /root/ui-cookies.txt --resolve n1:8443:127.0.0.1 --cacert {UI_CAFILE} https://n1:8443/cluster")
    assert "n3" in body, f"n3 missing from /cluster before the kill: {body}"

with subtest("kill n3 for real, start the stopwatch"):
    n3.crash()
    t0 = time.time()

with subtest("the alert really fires in Prometheus within the 30s budget"):
    def alert_firing():
        rc, out = n1.execute(
            f"curl -sf '{PROM}/api/v1/query' --data-urlencode "
            "'query=ALERTS{alertname=\"ExpanseNodeUnreachable\",alertstate=\"firing\",node=\"n3\"}'"
        )
        return rc == 0 and '"result":[{' in out

    elapsed_prom = poll_elapsed(t0, alert_firing, timeout=60, desc="ExpanseNodeUnreachable firing in Prometheus")
    print(f"Prometheus: ExpanseNodeUnreachable firing for n3 after {elapsed_prom:.1f}s")
    assert elapsed_prom <= BUDGET_SECONDS, f"Prometheus alert took {elapsed_prom:.1f}s, want <= {BUDGET_SECONDS}s"

with subtest("the in-cluster UI shows it live, over the already-open SSE connection, within budget"):
    def ui_shows_it():
        log = n1.succeed("cat /root/health-sse.log")
        return "ExpanseNodeUnreachable" in log and "n3" in log

    elapsed_ui = poll_elapsed(t0, ui_shows_it, timeout=60, desc="/health SSE stream showing ExpanseNodeUnreachable for n3")
    print(f"UI (/health, live SSE): ExpanseNodeUnreachable shown for n3 after {elapsed_ui:.1f}s")
    assert elapsed_ui <= BUDGET_SECONDS, f"UI took {elapsed_ui:.1f}s, want <= {BUDGET_SECONDS}s"

with subtest("the Grafana dashboard reflects it within budget, through Grafana's own query API"):
    def grafana_shows_it():
        values = grafana_ds_query(n1, 'expanse_quorum_node_unreachable{node="n3"}')
        return bool(values) and values[0] == 1

    elapsed_grafana = poll_elapsed(t0, grafana_shows_it, timeout=60, desc="Grafana's Per-node unreachable flag panel query reflecting n3")
    print(f"Grafana (Per-node unreachable flag panel query): reflected n3 after {elapsed_grafana:.1f}s")
    assert elapsed_grafana <= BUDGET_SECONDS, f"Grafana took {elapsed_grafana:.1f}s, want <= {BUDGET_SECONDS}s"

print(
    f"OBSERVABILITY-VERTICAL-SLICE DONE: alert={elapsed_prom:.1f}s ui={elapsed_ui:.1f}s "
    f"grafana={elapsed_grafana:.1f}s (budget={BUDGET_SECONDS}s)"
)
