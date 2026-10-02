"""block-liveness: see nix/tests/block-liveness.nix.

Runs after cluster-common.py and block-common.py.
"""

form("blockliveness")
for m in (n1, n2, n3):
    wait_agent_ready(m)

by_name = {"n1": n1, "n2": n2, "n3": n3}
unit = "expanse-block@default-web-0.service"


def liveness_record():
    """Replica 0's liveness record, or {} when its node has published none."""
    rc, out = kv(n1, "get /blocks/default/web/status/liveness/0")
    try:
        return json.loads(out) if rc == 0 else {}
    except ValueError:
        return {}


def invocation(m):
    return m.succeed(f"systemctl show -p InvocationID --value {unit}").strip()


with subtest("deploy whoami with a liveness probe"):
    deploy(n1, "web", (
        "apiVersion: expanse.io/v1\nkind: Block\n"
        "metadata:\n  name: web\n  namespace: default\n"
        "spec:\n  type: web/whoami\n  replicas: 1\n"
        "  resources:\n    requests:\n      cpu: 100m\n      memory: 64Mi\n"
        "  config:\n    port: 8080\n"
        "  network:\n    health_check:\n"
        "      readiness:\n        type: PROBE_TCP\n        port: 8080\n        period_seconds: 2\n"
        "      liveness:\n        type: PROBE_HTTP\n        port: 8080\n        path: /\n"
        "        period_seconds: 2\n        timeout_seconds: 1\n        failure_threshold: 2\n"
    ))
    b = wait_phase(n1, "web", ["RUNNING"], 120)
    node = replica_node(b, 0)
    host = by_name[node]
    assert liveness_record() == {}, f"a healthy replica has a liveness record: {liveness_record()}"
    before = invocation(host)

with subtest("a frozen replica is restarted on its node"):
    # SIGSTOP keeps the unit active and the port accepting, so only the http probe can tell.
    host.succeed(f"systemctl kill --signal=SIGSTOP {unit}")
    deadline = time.time() + 60
    rec = {}
    while time.time() < deadline:
        rec = liveness_record()
        if rec.get("restarts", 0) >= 1:
            break
        time.sleep(1)
    if rec.get("restarts", 0) < 1:
        print(kv(n1, "list /blocks/default/web/status/")[1])
        print(host.execute("journalctl -u expansed.service | grep -i liveness")[1])
    assert rec.get("restarts", 0) >= 1, f"replica never restarted: {rec}"
    assert rec.get("node") == node and not rec.get("failed"), f"unexpected record: {rec}"
    host.wait_until_succeeds("curl -sf --max-time 2 http://localhost:8080/", timeout=60)
    assert invocation(host) != before, "the unit was not restarted"
    host.succeed("journalctl -u expansed.service | grep -q 'liveness probe failing; restarting replica'")


def status_table():
    """The human `ctl block get` output, one list of fields per line."""
    out = n1.succeed(f"expanse ctl block get {SOCK} web")
    return out, [line.split() for line in out.splitlines()]


with subtest("the restarted replica stays put and RUNNING"):
    time.sleep(20)
    b = wait_phase(n1, "web", ["RUNNING"], 30)
    assert replica_node(b, 0) == node, f"replica moved: {b.get('status')}"
    assert liveness_record().get("restarts") == 1, f"restarted again: {liveness_record()}"

with subtest("ctl block get shows the replica's health by node"):
    health = next(p for p in b["status"]["placements"] if p.get("nodeId") == node).get("health") or {}
    assert health.get("restarts") == 1 and (health.get("readiness") or {}).get("ok"), f"health: {health}"
    assert b["status"].get("replicas", {}).get("ready") == 1, f"ready count: {b['status']}"
    out, rows = status_table()
    assert ["0", node, "RUNNING", "yes", "1"] in [r[:5] for r in rows], f"no healthy row for {node}:\n{out}"
    assert "Phase:   RUNNING (1/1 ready)" in out, out


def freeze_until(m, done, timeout):
    """SIGSTOP the replica on m every 2 s, so restarts freeze too, until done() is true."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        if done():
            return
        m.execute(f"systemctl kill --signal=SIGSTOP {unit}")
        time.sleep(2)
    print(kv(n1, "list /blocks/default/web/status/")[1])
    print(m.execute("journalctl -u expansed.service | grep -i liveness")[1])
    raise AssertionError(f"gave up freezing the replica on {m.name}: {get_json(n1, 'web')}")


def phase_of(b):
    return (b.get("status") or {}).get("phase", "")


with subtest("a replica still failing after its restarts moves to another node"):
    freeze_until(host, lambda: liveness_record().get("failed"), 120)
    assert liveness_record().get("node") == node, f"failed on the wrong node: {liveness_record()}"
    deadline = time.time() + 120
    b = get_json(n1, "web")
    while time.time() < deadline:
        b = get_json(n1, "web") or {}
        if replica_node(b, 0) not in (None, node) and phase_of(b) == "RUNNING":
            break
        time.sleep(2)
    second = replica_node(b, 0)
    assert second not in (None, node) and phase_of(b) == "RUNNING", f"replica not moved: {b.get('status')}"
    host.wait_until_fails(f"systemctl is-active --quiet {unit}", timeout=60)
    second_host = by_name[second]
    second_host.wait_until_succeeds("curl -sf --max-time 2 http://localhost:8080/", timeout=60)

with subtest("a replica failing on a second node is stopped, not moved again"):
    freeze_until(second_host, lambda: phase_of(get_json(n1, "web") or {}) == "FAILED", 180)
    b = get_json(n1, "web")
    reason = (b.get("status") or {}).get("pendingReason") or {}
    assert reason.get("code") == "LivenessFailed", f"unexpected reason: {b.get('status')}"
    for m in (n1, n2, n3):
        m.wait_until_fails(f"systemctl is-active --quiet {unit}", timeout=60)
    time.sleep(20)
    b = get_json(n1, "web")
    assert phase_of(b) == "FAILED" and replica_node(b, 0) is None, f"replica came back: {b.get('status')}"
    out, rows = status_table()
    assert "Reason:  LivenessFailed: replica 0 failed its liveness probe on 2 nodes" in out, out
    for n in (node, second):
        row = next((r for r in rows if r[1:2] == [n]), None)
        assert row and row[2] == "FAILED" and " ".join(row[5:]).startswith(
            "stopped: liveness probe failed after 1 restart:"), f"no stopped row for {n}:\n{out}"
