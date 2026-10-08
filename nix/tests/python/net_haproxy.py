"""net/haproxy: two stateless HAProxy replicas behind one VIP, across a backend and a node failure.

Deploys net/haproxy with two replicas on different nodes. Its http frontend
balances over two backends on the client and its tcp frontend sits on a second
VIP port. A stopped backend must drop out of rotation by health check and come
back when restarted; the stats port must report it. Then the node holding the
VIP is crashed and requests through the VIP must resume.

Runs after cluster-common.py (with client bound to n9), block-common.py and
vol_cluster.py (wait_for).
Expects TAG_BACKEND (net-haproxy.nix) spliced in ahead of this file; re comes
from cluster-common.py.
"""

NAME = "lb"
MACHINES = {"n1": n1, "n2": n2, "n3": n3}

MANIFEST = f"""apiVersion: expanse.io/v1
kind: Block
metadata:
  name: {NAME}
  namespace: default
spec:
  type: net/haproxy
  replicas: 2
  placement:
    antiAffinity: ANTI_AFFINITY_NODE
  resources:
    requests:
      cpu: 100m
      memory: 64Mi
  config:
    statsPort: 18404
    frontends:
      - name: web
        port: 18080
        servers: ["n9:8001", "n9:8002"]
        checkPath: /healthz
      - name: raw
        port: 18081
        mode: tcp
        servers: ["n9:8001"]
  network:
    ports:
      - name: http
        port: 80
        target_port: 18080
        protocol: tcp
        expose: EXPOSE_VIP
      - name: raw
        port: 81
        target_port: 18081
        protocol: tcp
        expose: EXPOSE_VIP
    health_check:
      readiness:
        type: PROBE_TCP
        port: 18080
        period_seconds: 2
"""


def vip_holders(vip, machines):
    """Nodes among machines carrying vip; never pass a crashed one (it would reboot)."""
    return [m.name for m in machines
            if m.execute(f"ip -4 -o addr show eth1 | grep -qF ' {vip}/'")[0] == 0]


def tags(n, port=80):
    """The backend tags of n requests through VIP:port; a failure is its HTTP code or curl exit."""
    out = []
    for _ in range(n):
        rc, body = client.execute(f"curl -s -m 3 -w '\\n%{{http_code}}' http://{VIP}:{port}/")
        text, _, code = body.rpartition("\n")
        out.append(text.split()[0] if rc == 0 and code == "200" else f"FAIL:{code}/rc{rc}")
    return out


def diagnose():
    print(client.execute(f"curl -sv -m 3 http://{VIP}/ 2>&1 | tail -15")[1])
    for node in sorted(placement_nodes(get_json(n1, NAME) or {})):
        print(f"--- {node}")
        print(MACHINES[node].execute(
            "getent ahosts n9; ss -ltnp | grep -E '1808|1840'; "
            "journalctl -u 'expanse-block@default-lb-*' --no-pager -n 40 2>&1")[1])


def metrics(node):
    return client.succeed(f"curl -sf -m 5 http://{IP[node]}:18404/metrics")


def active_servers(node, backend="be_web"):
    m = re.search(r'^haproxy_backend_active_servers\{proxy="' + backend + r'"[^}]*\} (\d+)',
                  metrics(node), re.MULTILINE)
    return int(m.group(1)) if m else -1


for port, tag in ((8001, "b1"), (8002, "b2")):
    client.succeed(f"systemd-run --unit=backend-{tag} python3 {TAG_BACKEND} {port} {tag}")
    client.wait_for_open_port(port)

form("haproxy")
for m in MACHINES.values():
    wait_agent_ready(m)

with subtest("deploy two net/haproxy replicas on different nodes"):
    deploy(n1, NAME, MANIFEST)
    b = wait_phase(n1, NAME, ["RUNNING"], 180)
    nodes = placement_nodes(b)
    assert len(nodes) == 2, f"{NAME} placed on {nodes}: {b.get('status')}"
    VIP = wait_block_vip(n1, NAME)
    wait_for(lambda: len(vip_holders(VIP, MACHINES.values())) == 1, f"VIP {VIP} held", timeout=60)

with subtest("the http frontend balances over both backends and adds X-Forwarded-For"):
    try:
        client.wait_until_succeeds(f"curl -sf -m 3 http://{VIP}/", timeout=60)
        seen = tags(10)
        assert set(seen) == {"b1", "b2"}, f"not balanced over both backends: {seen}"
    except Exception:
        diagnose()
        raise
    body = client.succeed(f"curl -sf -m 3 http://{VIP}/")
    assert "xff=192.168.1." in body, f"no X-Forwarded-For: {body!r}"

with subtest("the tcp frontend answers on the block's second VIP port"):
    assert tags(4, port=81) == ["b1"] * 4, "tcp frontend on VIP:81"

with subtest("each replica reports both servers up on its stats port"):
    for node in nodes:
        wait_for(lambda node=node: active_servers(node) == 2, f"{node} sees 2 active servers", timeout=30)
        client.succeed(f"curl -sf -m 5 -o /tmp/stats http://{IP[node]}:18404/stats && grep -q be_web /tmp/stats")

with subtest("a stopped backend leaves rotation without failed requests"):
    client.succeed("systemctl stop backend-b2")
    for node in nodes:
        wait_for(lambda node=node: active_servers(node) == 1, f"{node} marks b2 down", timeout=30)
    seen = tags(20)
    assert seen == ["b1"] * 20, f"requests after b2 stopped: {seen}"

with subtest("the restarted backend rejoins"):
    client.succeed(f"systemd-run --unit=backend-b2-again python3 {TAG_BACKEND} 8002 b2")
    for node in nodes:
        wait_for(lambda node=node: active_servers(node) == 2, f"{node} marks b2 up", timeout=30)
    # HAProxy's weighted round robin lets a returning server catch up, so a burst may all go to b2.
    seen = tags(10)
    if "b2" not in seen or not set(seen) <= {"b1", "b2"}:
        diagnose()
        raise AssertionError(f"b2 did not rejoin: {seen}")

with subtest("crash the node holding the VIP"):
    holder = vip_holders(VIP, MACHINES.values())[0]
    t0 = time.time()
    MACHINES[holder].crash()
    survivors = [m for n, m in MACHINES.items() if n != holder]

with subtest("requests through the VIP resume on a survivor"):
    deadline = time.time() + 240
    while time.time() < deadline:
        if client.execute(f"curl -sf -m 2 http://{VIP}/")[0] == 0:
            break
        time.sleep(1)
    else:
        raise AssertionError(f"VIP {VIP} never answered again in 240s")
    print(f"VIP answered again {time.time() - t0:.1f}s after the crash, held by "
          f"{vip_holders(VIP, survivors)}")
    wait_for(lambda: set(tags(10)) == {"b1", "b2"}, "both backends served after the crash", timeout=60)

with subtest("the lost replica is rescheduled onto the spare node"):
    deadline = time.time() + 240
    cur = set()
    while time.time() < deadline:
        b = get_json(survivors[0], NAME) or {}
        cur = placement_nodes(b)
        live = {i: ph for i, ph in replica_phases(b).items() if i >= 0}  # the lost one stays listed
        if len(cur) == 2 and holder not in cur and len(live) == 2 and all(ph == "RUNNING" for ph in live.values()):
            break
        time.sleep(2)
    else:
        raise AssertionError(f"replicas never re-placed on the survivors: {sorted(cur)} {b.get('status')}")
    print(f"replicas RUNNING on {sorted(cur)} {time.time() - t0:.1f}s after the crash")
    print("NET-HAPROXY DONE")
