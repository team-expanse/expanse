"""observability-metrics: PHASE-09-TASKS.md Stream A (X1). A real
Prometheus binary scrapes a real node's /metrics endpoint over mTLS with
bearer-token auth (D2) and the exported node/resource/volume/quorum
health samples are queried back out of Prometheus's own HTTP API -- not
assumed from "the endpoint returned 200".

Runs after cluster-common.py; NODES/n1/n2/n3/form/wait_quorum/
wait_agent_ready all come from that shared file.
"""

import json

SOCK = "/run/expanse/agent.sock"
TOKEN = "vm-test-metrics-token-do-not-log-me"
CAFILE = "/root/expanse-ca.pem"
PROM = "http://127.0.0.1:9090"


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


form("test")

with subtest("real desired state: a file resource on n1"):
    apply_file(n1, "file:/etc/metrics-probe", "/etc/metrics-probe", "alpha")
    n1.wait_until_succeeds("grep -qx alpha /etc/metrics-probe", timeout=60)

with subtest("a real replicated volume on n1"):
    n1.succeed("expanse ctl volume create mvol --size 64Mi --replication 3")

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
        # Node certs carry DNS-name SANs (node ID + hostname), no IP SANs
        # (internal/cluster/ca.IssueNode's own doc, D2) -- verify against
        # the node's real name, the same --resolve trick ui_auth.py uses.
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

with subtest("node health is really exported, per check"):
    result = prom_query(n1, "expanse_node_health")
    assert result, "expanse_node_health returned no series"
    checks = {r["metric"]["check"] for r in result}
    assert "disk-space" in checks, f"expected a 'disk-space' check series, got {checks}"

with subtest("the file resource's health is really exported"):
    result = prom_query(n1, 'expanse_resource_health{id="file:/etc/metrics-probe"}')
    assert result, "no expanse_resource_health series for the file resource"
    assert result[0]["value"][1] == "1", f"file resource health = {result[0]['value']}, want healthy (1)"

with subtest("the volume's health is really exported, and reaches Healthy"):
    # 2 == VOLUME_STATE_HEALTHY (proto/storage.proto's own ordinal, D3).
    n1.wait_until_succeeds(
        f"curl -sf '{PROM}/api/v1/query' --data-urlencode 'query=expanse_volume_health{{volume=\"mvol\"}}' "
        "| grep -q '\"2\"'",
        timeout=60,
    )
    result = prom_query(n1, 'expanse_volume_health{volume="mvol"}')
    assert result[0]["metric"]["state"] == "Healthy", f"volume state label = {result[0]['metric']}"

with subtest("quorum health reflects the real 3-node cluster"):
    result = prom_query(n1, "expanse_quorum_voters")
    assert result and result[0]["value"][1] == "3", f"expanse_quorum_voters = {result}"
    result = prom_query(n1, "expanse_quorum_has_leader")
    assert result and result[0]["value"][1] == "1", f"expanse_quorum_has_leader = {result}"

# --resolve pins the TLS verification hostname to "n1" (the cert's real
# DNS SAN) while still dialing loopback, the same trick ui_auth.py uses.
CURL = f"curl -s -o /dev/null -w '%{{http_code}}' --resolve n1:7447:127.0.0.1 --cacert {CAFILE}"

with subtest("a wrong bearer token is rejected -- auth is real, not a no-op"):
    code = n1.succeed(f"{CURL} -H 'Authorization: Bearer wrong-token' https://n1:7447/metrics").strip()
    assert code == "401", f"wrong bearer token returned {code}, want 401"

with subtest("no bearer token at all is rejected"):
    code = n1.succeed(f"{CURL} https://n1:7447/metrics").strip()
    assert code == "401", f"missing bearer token returned {code}, want 401"

with subtest("the correct bearer token authorizes a direct scrape"):
    code = n1.succeed(f"{CURL} -H 'Authorization: Bearer {TOKEN}' https://n1:7447/metrics").strip()
    assert code == "200", f"correct bearer token returned {code}, want 200"

print("OBSERVABILITY-METRICS DONE")
