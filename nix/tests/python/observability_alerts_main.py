"""observability-alerts: PHASE-09-TASKS.md Stream B (X2). The shipped
alert rules (deploy/prometheus/expanse-alerts.rules.yml) are loaded by a
real Prometheus and observed to actually transition to firing under
genuinely degraded fixtures on a real 3-node cluster -- not just "the
rule file parses" (that's promtool's own job, see
deploy/prometheus/expanse-alerts.rules_test.yml, run outside this VM).

Runs after cluster-common.py; NODES/n1/n2/n3/form/wait_quorum/
wait_agent_ready come from that shared file. RULES_FILE is injected by
observability-alerts.nix (the rule file's real nix store path).
"""

import json

SOCK = "/run/expanse/agent.sock"
TOKEN = "vm-test-alerts-token-do-not-log-me"
CAFILE = "/root/expanse-ca.pem"
PROM = "http://127.0.0.1:9090"
BAD_PATH = "/nonexistent-parent-dir-xyz/probe"


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


def wait_alert_firing(m, alertname, timeout=90):
    """Polls Prometheus's own ALERTS series (the real alert-evaluation
    output, not the rule file) until alertname reaches state=firing."""
    m.wait_until_succeeds(
        "curl -sf '"
        + PROM
        + f"/api/v1/query' --data-urlencode 'query=ALERTS{{alertname=\"{alertname}\",alertstate=\"firing\"}}' "
        "| grep -q '\"result\":\\[{'",
        timeout=timeout,
    )


form("test")

with subtest("a real resource that can never reconcile: no such parent directory"):
    apply_file(n1, f"file:{BAD_PATH}", BAD_PATH, "unreachable")
    # atomicWrite's os.CreateTemp(dir, ...) fails for real when dir does
    # not exist -- this resource is genuinely, durably unhealthy, not a
    # forced/fabricated value.
    n1.wait_until_succeeds(
        f"expanse ctl resource get --socket {SOCK} 'file:{BAD_PATH}' | grep -qi unhealthy", timeout=60
    )

with subtest("set a known metrics token and a world-readable CA copy"):
    n1.succeed(f"expanse ctl metrics set-token --token '{TOKEN}'")
    n1.succeed("install -m0644 /persist/expanse/ca/ca.pem " + CAFILE)
    n1.succeed(f"printf '%s' '{TOKEN}' > /root/metrics-token && chmod 0644 /root/metrics-token")
    n1.succeed(f"install -m0644 {RULES_FILE} /root/expanse-alerts.rules.yml")

with subtest("a real prometheus loads the shipped rule file and scrapes n1"):
    n1.succeed(
        "cat > /root/prometheus.yml <<'EOF'\n"
        "global:\n"
        # Fast eval so the test doesn't wait on a real-world 10-15s (D6)
        # cadence; for: 0s rules still fire on the first true evaluation
        # regardless of this interval.
        "  scrape_interval: 2s\n"
        "  evaluation_interval: 5s\n"
        "rule_files:\n"
        "  - /root/expanse-alerts.rules.yml\n"
        "scrape_configs:\n"
        "  - job_name: expanse\n"
        "    scheme: https\n"
        "    tls_config:\n"
        f"      ca_file: {CAFILE}\n"
        # Node certs carry DNS-name SANs only, no IP SANs
        # (internal/cluster/ca.IssueNode, Stream A's own finding) --
        # verify against the node's real name.
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
    n1.wait_until_succeeds(
        f"curl -sf {PROM}/api/v1/rules | grep -q ExpanseResourceUnhealthy", timeout=60
    )

with subtest("the unreconcilable resource's bad health is really scraped"):
    result = prom_query(n1, f'expanse_resource_health{{id="file:{BAD_PATH}"}}')
    assert result and result[0]["value"][1] == "3", f"resource health = {result}, want unhealthy (3)"

with subtest("ExpanseResourceUnhealthy really fires -- not just parses"):
    wait_alert_firing(n1, "ExpanseResourceUnhealthy")

with subtest("a real quorum loss: stop expansed on 2 of the 3 nodes"):
    n2.succeed("systemctl stop expansed.service")
    n3.succeed("systemctl stop expansed.service")
    n1.wait_until_succeeds(
        f"curl -sf '{PROM}/api/v1/query' --data-urlencode 'query=expanse_quorum_has_leader' "
        "| grep -q '\"0\"'",
        timeout=60,
    )
    result = prom_query(n1, "expanse_quorum_degraded")
    assert result and result[0]["value"][1] == "1", f"expanse_quorum_degraded = {result}, want 1"

with subtest("ExpanseQuorumNoLeader and ExpanseQuorumDegraded really fire"):
    wait_alert_firing(n1, "ExpanseQuorumNoLeader")
    wait_alert_firing(n1, "ExpanseQuorumDegraded")

print("OBSERVABILITY-ALERTS DONE")
