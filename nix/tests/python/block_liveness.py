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

with subtest("the restarted replica stays put and RUNNING"):
    time.sleep(20)
    b = wait_phase(n1, "web", ["RUNNING"], 30)
    assert replica_node(b, 0) == node, f"replica moved: {b.get('status')}"
    assert liveness_record().get("restarts") == 1, f"restarted again: {liveness_record()}"
