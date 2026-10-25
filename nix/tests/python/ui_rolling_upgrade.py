"""ui-rolling-upgrade (Phase 15 U2/U3): the Upgrades page rolls a 3-node cluster to a release.

`rel` serves a signed test release whose node-system.nix is each node's prebuilt
specialisation (expanse 99.0.0), so `expanse upgrade apply` runs for real but builds nothing.
Runs after cluster-common.py, block-common.py, vol_cluster.py and ui_forms_common.py.
"""

PASSWORD = "ui-rolling-upgrade-password"
TARGET = "99.0.0"
APP_UNIT = "expanse-block@default-app-0.service"

APP_MANIFEST = """apiVersion: expanse.io/v1
kind: Block
metadata:
  name: app
  namespace: default
spec:
  type: util/echo
  replicas: 1
  strategy:
    kind: SINGLETON
  resources:
    requests:
      cpu: 100m
      memory: 64Mi
  storage:
    - name: data
      size: 32Mi
      mountPath: /mnt/app-data
  config:
    port: 18090
    body: "rolled\\n"
"""


def upgrade_status(m):
    rc, out = m.execute("expanse upgrade status")
    return json.loads(out) if rc == 0 and out.lstrip().startswith("{") else None


def plan():
    rc, out = n1.execute("expanse ctl kv --socket /run/expanse/agent.sock get /upgrade/plan 2>/dev/null")
    found = re.search(r"\{.*\}", out, re.S)
    return json.loads(found.group(0)) if rc == 0 and found else {}


def on_target(m):
    return TARGET in m.execute("expanse version")[1]


def app_host():
    b = get_json(n1, "app")
    return {"n1": n1, "n2": n2, "n3": n3}[replica_node(b, 0)]


def page(path):
    return n1.succeed(f"{CURL} https://n1:8443{path}")


TRACING = "/sys/kernel/tracing"
KILL_EVENT = f"{TRACING}/events/signal/signal_generate"


def arm_kill_trace(m):
    """Record every SIGKILL with its sender and kernel stack (the upgrade build's unknown killer)."""
    m.succeed(f"mountpoint -q {TRACING} || mount -t tracefs nodev {TRACING}",
              f"echo 8192 > {TRACING}/buffer_size_kb",
              f"echo 'sig == 9' > {KILL_EVENT}/filter",
              f"echo 'stacktrace if sig == 9' > {KILL_EVENT}/trigger",
              f"echo 1 > {KILL_EVENT}/enable")


def print_kill_trace(m):
    print(f"--- {m.name} SIGKILLs ---")
    print(m.execute(f"cat {TRACING}/trace")[1])
    print(m.execute("journalctl -u expanse-upgrade --no-pager | grep -B3 -A3 'signal' | tail -40")[1])


def dump_on_failure():
    print(plan())
    for m in [n1, n2, n3]:
        print(m.name, upgrade_status(m))
        print(m.execute("journalctl -u expanse-upgrade --no-pager | tail -40")[1])
        print(m.execute("journalctl -u expansed --no-pager | grep -i -E 'rollout|upgrade' | tail -20")[1])


try:
    with subtest("a 3-node cluster runs a block with storage"):
        rel.start()
        form()
        rel.wait_for_open_port(8000)
        deploy(n1, "app", APP_MANIFEST, port=18090)
        wait_phase(n1, "app", ["RUNNING"], 240)
        host = app_host()
        pid = host.succeed(f"systemctl show -P MainPID {APP_UNIT}").strip()
        leader = leader_of(status(n1))
        assert leader == "n1", f"leader is {leader}"

    with subtest("the Upgrades page offers the published releases and flags the update"):
        n1.succeed(f"expanse ctl admin reset-password --password '{PASSWORD}'")
        csrf = ui_login()
        latest = json.dumps({"version": TARGET, "releases": [TARGET, "1.0.0"], "disabled": True})
        n1.succeed(f"expanse ctl kv --socket /run/expanse/agent.sock put /upgrade/latest {shlex.quote(latest)}")
        body = page("/upgrade")
        assert f'<option value="{TARGET}" selected>{TARGET} (newest)</option>' in body, body
        assert 'value="1.0.0"' not in body, "offered a release older than the cluster's"
        assert 'class="nav-badge"' in body and f">{TARGET} available<" in body, "no update indicator"
        assert body.count("Update available") == 3, "not every node is marked behind"

    with subtest("Upgrade cluster on the Upgrades page starts a rollout"):
        for m in [n1, n2, n3]:
            arm_kill_trace(m)
        code, loc, body = post("/upgrade/start", {"version": TARGET})
        assert code == "303", f"start: {code}\n{body}"

    with subtest("every node reaches the release, one at a time, the leader last"):
        for m in [n1, n2, n3]:
            m.wait_until_succeeds(f"expanse version | grep -q {TARGET}", timeout=900)
        wait_for(lambda: plan().get("state") == "done", "the rollout to finish", 300)
        st = {m.name: upgrade_status(m) for m in [n1, n2, n3]}
        assert all(s and s["state"] == "done" and s["version"] == TARGET for s in st.values()), st
        sec = {n: (s["started"][:19], s["updated"][:19]) for n, s in st.items()}  # RFC 3339, to the second
        assert sec["n2"][1] <= sec["n3"][0] and sec["n3"][1] <= sec["n1"][0], \
            f"nodes overlapped or ran out of order (want n2, n3, n1): {sec}"

    with subtest("the block kept running on its node through the rollout"):
        assert host.succeed(f"systemctl show -P MainPID {APP_UNIT}").strip() == pid, "the block restarted"
        wait_phase(n1, "app", ["RUNNING"], 60)
        wait_quorum("3/2", 60)

    with subtest("the update indicator clears once every node runs the release"):
        csrf = ui_login()
        assert "nav-badge" not in page("/"), "a cluster on the newest release still shows an update"
except Exception:
    dump_on_failure()
    raise
finally:
    for m in [n1, n2, n3]:
        print_kill_trace(m)

print("UI-ROLLING-UPGRADE PASSED: the cluster rolled to the release node by node; the block kept running")
