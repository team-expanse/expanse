"""monitor/uptime-kuma: status monitoring that keeps every acknowledged monitor after losing its node.

Deploys a SINGLETON monitor/uptime-kuma block on a 3-way volume behind a VIP on 80.
A client creates the administrator, adds an HTTP monitor of a web server on n9 that
must report UP, adds push monitors one acknowledged at a time and the serving VM is
crashed right after the last; the block, its volume and its VIP must re-converge on
a survivor that has every monitor, still accepts the session token issued before
the crash, keeps monitoring and saves more.

Runs after cluster-common.py (with client bound to n9), block-common.py and
vol_cluster.py.
"""

NAME = "uptime"
MACHINES = {"n1": n1, "n2": n2, "n3": n3}
MONITORS = 20

MANIFEST = f"""apiVersion: expanse.io/v1
kind: Block
metadata:
  name: {NAME}
  namespace: default
spec:
  type: monitor/uptime-kuma
  replicas: 1
  strategy:
    kind: SINGLETON
  resources:
    requests:
      cpu: 250m
      memory: 256Mi
  storage:
    - name: uptime-kuma-data
      size: 1Gi
      replication: 3
      mountPath: /var/lib/uptime-kuma
  network:
    ports:
      - name: http
        port: 80
        target_port: 3001
        protocol: tcp
        expose: EXPOSE_VIP
    health_check:
      readiness:
        type: PROBE_TCP
        port: 3001
        period_seconds: 2
"""


def vip_holders(vip, machines):
    """Nodes among machines carrying vip; never pass a crashed one (it would reboot)."""
    return [m.name for m in machines
            if m.execute(f"ip -4 -o addr show eth1 | grep -qF ' {vip}/'")[0] == 0]


def kuma(*args):
    """One uptime_kuma_client.py command against the VIP, decoded from its JSON output."""
    quoted = " ".join(f"'{a}'" for a in args)
    return json.loads(client.succeed(f"kuma http://{VIP} admin Admin-password-4-tests {quoted}"))


def journal(m):
    return m.execute(f"journalctl -t 'expanse-block-default-{NAME}-0' --no-pager -n 200 2>&1")[1]


def wait_answering(m):
    try:
        client.wait_until_succeeds(f"kuma http://{VIP} admin Admin-password-4-tests setup", timeout=240)
    except Exception:
        print(journal(m))
        raise


form("uptime-kuma")
for m in MACHINES.values():
    wait_agent_ready(m)

with subtest("a manifest with a privileged port is rejected"):
    bad = MANIFEST.replace("  network:", "  config:\n    port: 80\n  network:")
    n1.succeed(f"echo {base64.b64encode(bad.encode()).decode()} | base64 -d > /tmp/bad.yaml")
    out = n1.fail(f"expanse ctl block apply {SOCK} -f /tmp/bad.yaml 2>&1")
    assert "port" in out, f"rejection does not name port: {out}"

with subtest("deploy a SINGLETON monitor/uptime-kuma block behind a VIP"):
    deploy(n1, NAME, MANIFEST)
    b = wait_phase(n1, NAME, ["RUNNING"], 240)
    nodes = placement_nodes(b)
    assert len(nodes) == 1, f"{NAME} placed on {nodes}: {b.get('status')}"
    holder = next(iter(nodes))
    VIP = wait_block_vip(n1, NAME)
    for m in NODES:
        m.wait_until_succeeds("drbdadm status | grep -q '^vol-'", timeout=180)
    res = n1.succeed("drbdadm status | head -1 | cut -d' ' -f1").strip()
    wait_for(lambda: all(fully_replicated(m, res) for m in NODES), "every replica UpToDate")
    wait_for(lambda: vip_holders(VIP, NODES) == [holder], f"VIP {VIP} on {holder}", timeout=60)

with subtest("Uptime Kuma answers on SQLite with every commit synced"):
    wait_answering(MACHINES[holder])
    assert "<html" in client.succeed(f"curl -sfL -m 10 http://{VIP}/").lower(), "the web UI is not served"
    log = journal(MACHINES[holder])
    assert "synchronous pragmas forced to FULL" in log, f"the SQLite preload did not load:\n{log}"
    cfg = MACHINES[holder].succeed("cat /var/lib/expanse/volumes/*/mnt/uptime-kuma/db-config.json")
    assert json.loads(cfg)["type"] == "sqlite", cfg

with subtest("an HTTP monitor of a web server on n9 reports UP"):
    token = kuma("login")
    web = kuma("add-http", "n9 web", "http://n9:8080/")
    kuma("wait-up", str(web), "90")

with subtest("crash the serving node right after monitors are acknowledged"):
    created = set(kuma("add-push", "push", str(MONITORS)))
    t0 = time.time()
    MACHINES[holder].crash()
    survivors = [m for n, m in MACHINES.items() if n != holder]

with subtest("block, volume primary and VIP re-converge on one survivor"):
    new_holder = None
    last_seen = {}
    deadline = time.time() + 240
    while time.time() < deadline and new_holder is None:
        cur = placement_nodes(get_json(survivors[0], NAME) or {})
        if len(cur) == 1 and holder not in cur:
            cand = next(iter(cur))
            last_seen = {"placement": cand, "role": role_of(MACHINES[cand], res),
                         "vip": vip_holders(VIP, survivors)}
            if last_seen["role"] == "Primary" and last_seen["vip"] == [cand]:
                new_holder = cand
        else:
            last_seen = {"placement": sorted(cur)}
        time.sleep(2)
    assert new_holder, f"never re-converged on one survivor in 240s: {last_seen}"
    print(f"{NAME} re-converged on {new_holder} after {time.time() - t0:.1f}s")

with subtest("the survivor has every acknowledged monitor and honours the pre-crash token"):
    wait_answering(MACHINES[new_holder])
    print(f"Uptime Kuma answered again {time.time() - t0:.1f}s after the crash")
    names = set(kuma("list").values())
    missing = created - names
    assert not missing, f"{len(missing)} of {MONITORS} acknowledged monitors lost across failover: {sorted(missing)}"
    assert "n9 web" in names, "the HTTP monitor was lost"
    assert kuma("token-ok", token), "the session token issued before the crash was refused"

with subtest("the survivor keeps monitoring and saves changes"):
    kuma("wait-up", str(web), "90")
    kuma("add-http", "after failover", "http://n9:8080/")
    assert "after failover" in kuma("list").values()
    print("MONITOR-UPTIME-KUMA DONE")
