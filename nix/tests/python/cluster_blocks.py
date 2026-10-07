"""NODE_COUNT nodes running the monitoring and NFS blocks together.

Forms the cluster, deploys a node-exporter daemonset, a SINGLETON
Prometheus and Grafana, and a SINGLETON share/nfs export behind a VIP that
zclient writes to through one held descriptor. Then crashes the node
serving the export: the export, its volume and its VIP move to a survivor,
the client reclaims its state, writes resume, the volume matches the
client, and Prometheus sees the dead node's targets go down.

Runs after cluster-common.py and block-common.py; expects NODE_COUNT.
"""
import functools

print = functools.partial(print, flush=True)  # stdout is a file; show timings as they happen

NODES = {f"n{i}": globals()[f"n{i}"] for i in range(1, NODE_COUNT + 1)}
QUORUM = (NODE_COUNT, NODE_COUNT // 2 + 1)
client = zclient
TOKEN = "cluster-blocks-metrics-token"
PROM_PORT, GRAFANA_PORT, NODEEXP_PORT = 18110, 18111, 18112
NFS_PORT, GANESHA_PORT = 2049, 12049
ADMIN_PW = "cluster-blocks-admin"
STREAM_FILE = "/mnt/client/stream.txt"
NFS_VOL = "blk-default-files-files-data"  # storage.BlockVolumeName(ns, block, storageName)
DOWN = set()  # crashed nodes: never execute() on them, the driver would reboot them


def live():
    return [m for n, m in NODES.items() if n not in DOWN]


def via():
    return live()[0]


def until(predicate, what, timeout):
    """Poll predicate every second; return seconds taken."""
    t0 = time.time()
    while time.time() - t0 < timeout:
        if predicate():
            return time.time() - t0
        time.sleep(1)
    raise AssertionError(f"timed out after {timeout}s waiting for {what}")


def quorum(m):
    found = re.search(r"quorum:\s+(\d+)/(\d+)", status(m))
    return (int(found.group(1)), int(found.group(2))) if found else (0, 0)


def block_yaml(name, typ, config, storage=True, strategy="SINGLETON", extra=""):
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
        f"{st}  config:\n{cfg}{extra}"
    )


def holder(name):
    """The node running the single replica of name, or None while it moves."""
    b = get_json(via(), name)
    nodes = placement_nodes(b) if b else set()
    return next(iter(nodes)) if len(nodes) == 1 else None


def prom_try(path):
    """Decoded Prometheus API response, or None while it is down or moving."""
    node = holder("prom")
    if node is None or node in DOWN:
        return None
    rc, out = via().execute(f"curl -sf 'http://{addr(NODES[node])}:{PROM_PORT}{path}'")
    return json.loads(out) if rc == 0 else None


def targets():
    """job/node -> health for every active scrape target; empty while down."""
    doc = prom_try("/api/v1/targets?state=active")
    if doc is None:
        return {}
    return {f"{t['labels']['job']}/{t['labels'].get('node', '')}": t["health"]
            for t in doc["data"]["activeTargets"]}


def voters_seen():
    doc = prom_try("/api/v1/query?query=max(expanse_quorum_voters)")
    res = doc["data"]["result"] if doc else []
    return res[0]["value"][1] if res else None


def vip_holders(vip):
    return [m.name for m in live() if m.execute(f"ip -4 -o addr show eth1 | grep -qF ' {vip}/'")[0] == 0]


def last_acked():
    out = client.execute("cat /root/last_acked 2>/dev/null || echo 0")[1].strip()
    return int(out) if out.isdigit() else 0


def nfs_journal(m):
    return m.execute("journalctl -u 'expanse-block-root@default-files-0.service' --no-pager 2>&1")[1]


def nfs_host_path(m):
    for line in m.execute("expanse ctl volume list 2>&1")[1].splitlines():
        cols = line.split()
        if len(cols) >= 6 and cols[1] == NFS_VOL and cols[0] != "-":
            return f"/var/lib/expanse/volumes/{cols[0]}/mnt/share"
    return None


with subtest(f"form a {NODE_COUNT}-node cluster"):
    t0 = time.time()
    start_all()
    for m in NODES.values():
        m.wait_for_unit("multi-user.target")
        m.succeed("systemctl stop expansed.service")
    n1.succeed("expanse cluster init --data-dir /persist/expanse --name blocks --node-id n1 "
               f"--advertise-addr {addr(n1)}:7444 --expect {NODE_COUNT}")
    token = ""
    for _ in range(30):
        _, out = n1.execute(f"expanse cluster token --data-dir /persist/expanse create --uses {NODE_COUNT - 1} 2>/dev/null || true")
        found = re.search(r"expanse-join-[A-Za-z0-9_-]+", out)
        if found:
            token = found.group(0)
            break
        time.sleep(1)
    assert token, "no join token minted"
    n1.succeed("systemctl start expansed.service")
    wait_agent_ready(n1)
    for name in list(NODES)[1:]:
        join_and_start(NODES[name], token, "voter")
    until(lambda: quorum(n1) == QUORUM, f"quorum {QUORUM}", 300)
    for m in NODES.values():
        wait_agent_ready(m)
    print(f"TIMING form {NODE_COUNT} nodes to quorum {QUORUM}: {time.time() - t0:.1f}s")
    n1.succeed(f"expanse ctl metrics set-token --token '{TOKEN}'")

with subtest(f"node-exporter runs on all {NODE_COUNT} nodes"):
    t0 = time.time()
    deploy(n1, "nodeexp", block_yaml("nodeexp", "monitor/node-exporter", {"port": NODEEXP_PORT},
                                     storage=False, strategy="DAEMONSET"))
    wait_phase(n1, "nodeexp", ["RUNNING"], 300)
    for m in NODES.values():
        m.wait_until_succeeds(f"curl -sf http://127.0.0.1:{NODEEXP_PORT}/metrics | grep node_cpu >/dev/null", timeout=300)
    print(f"TIMING node-exporter on every node: {time.time() - t0:.1f}s")

with subtest(f"Prometheus scrapes all {NODE_COUNT} agents and node-exporters"):
    t0 = time.time()
    deploy(n1, "prom", block_yaml("prom", "monitor/prometheus", {
        "port": PROM_PORT, "metricsToken": TOKEN, "scrapeInterval": "5s", "nodeExporterPort": NODEEXP_PORT,
    }))
    wait_phase(n1, "prom", ["RUNNING"], 300)
    want = {f"expanse-{n}/{n}" for n in NODES} | {f"node-exporter/{n}" for n in NODES} | {"prometheus/"}
    got = {}

    def all_up():
        global got
        got = targets()
        return want <= set(got) and all(got[k] == "up" for k in want)
    try:
        until(all_up, "every target up", 300)
    except AssertionError:
        raise AssertionError(f"targets not all up: missing {sorted(want - set(got))}, "
                             f"down {sorted(k for k in want & set(got) if got[k] != 'up')}") from None
    until(lambda: voters_seen() == str(NODE_COUNT), f"expanse_quorum_voters == {NODE_COUNT}", 120)
    print(f"TIMING prometheus: {len(want)} targets up, {NODE_COUNT} voters seen: {time.time() - t0:.1f}s")

with subtest("Grafana serves the shipped dashboard with live data"):
    prom_url = f"http://{addr(NODES[holder('prom')])}:{PROM_PORT}"
    deploy(n1, "grafana", block_yaml("grafana", "monitor/grafana", {
        "port": GRAFANA_PORT, "prometheusUrl": prom_url, "adminPassword": ADMIN_PW,
    }))
    wait_phase(n1, "grafana", ["RUNNING"], 300)
    GRAFANA = f"http://{addr(NODES[holder('grafana')])}:{GRAFANA_PORT}"
    n1.wait_until_succeeds(f"curl -sf {GRAFANA}/api/health | grep '\"database\": \"ok\"' >/dev/null", timeout=180)
    n1.wait_until_succeeds(f"curl -sf -u admin:{ADMIN_PW} {GRAFANA}/api/dashboards/uid/expanse-health "
                           "| grep 'Node Health' >/dev/null", timeout=60)
    body = json.dumps({
        "queries": [{"refId": "A", "datasource": {"type": "prometheus", "uid": "expanse-prometheus"},
                     "expr": "max(expanse_quorum_voters)", "instant": True, "range": False}],
        "from": "now-5m", "to": "now",
    })
    out = n1.succeed(f"curl -sf -u admin:{ADMIN_PW} -H 'Content-Type: application/json' -d '{body}' "
                     f"{GRAFANA}/api/ds/query")
    values = json.loads(out)["results"]["A"]["frames"][0]["data"]["values"][-1]
    assert values and values[0] == NODE_COUNT, f"voters via grafana = {values}"

with subtest("an NFS export behind a VIP takes writes from an outside client"):
    deploy(n1, "files", block_yaml("files", "share/nfs", {"port": GANESHA_PORT, "gracePeriod": 30}, extra=f"""\
  network:
    ports:
      - name: nfs
        port: {NFS_PORT}
        target_port: {GANESHA_PORT}
        protocol: tcp
        expose: EXPOSE_VIP
    health_check:
      readiness:
        type: PROBE_TCP
        port: {GANESHA_PORT}
        period_seconds: 2
"""))
    wait_phase(n1, "files", ["RUNNING"], 300)
    nfs_node = holder("files")
    VIP = wait_block_vip(n1, "files")
    until(lambda: vip_holders(VIP) == [nfs_node], f"VIP {VIP} on {nfs_node}", 90)
    client.succeed("mkdir -p /mnt/client")
    client.wait_until_succeeds(f"mount -t nfs4 -o vers=4.1,hard {VIP}:/share /mnt/client", timeout=90)
    client.succeed(
        "cat > /root/writer.sh << 'EOF'\n"
        "export PATH=/run/current-system/sw/bin:$PATH\n"
        f"exec 3>> {STREAM_FILE}\n"
        "i=0\n"
        "while true; do\n"
        "  i=$((i+1))\n"
        "  if printf 'seq %06d\\n' \"$i\" >&3; then echo \"$i\" > /root/last_acked; fi\n"
        "  sleep 0.3\n"
        "done\n"
        "EOF\n"
    )
    client.succeed("systemd-run --unit=nfs-writer /bin/sh /root/writer.sh")
    until(lambda: last_acked() >= 5, "writes flowing", 60)
    print(f"PLACEMENT files={nfs_node} prom={holder('prom')} grafana={holder('grafana')} VIP={VIP}")

with subtest("kill the node serving the export mid-write"):
    acked_at_kill = last_acked()
    t_kill = time.time()
    NODES[nfs_node].crash()
    DOWN.add(nfs_node)
    print(f"KILLED {nfs_node} at {acked_at_kill} acked writes")

with subtest("the export, its volume and its VIP move to one survivor"):
    def moved():
        h = holder("files")
        return h not in (None, nfs_node) and vip_holders(VIP) == [h]
    print(f"TIMING export and VIP on a survivor: {until(moved, 'export moved', 300):.1f}s")
    new_node = holder("files")

with subtest("the client reclaims its state and the held descriptor keeps writing"):
    try:
        until(lambda: last_acked() >= acked_at_kill + 5, "post-failover writes", 300)
    except AssertionError:
        print(nfs_journal(NODES[new_node])[-4000:])
        raise
    print(f"TIMING writes resumed after the kill: {time.time() - t_kill:.1f}s")
    assert "reclaim complete(1) clid count(1)" in nfs_journal(NODES[new_node]), "survivor never saw the reclaim"

with subtest("the volume agrees byte for byte with the client"):
    client.succeed("systemctl stop nfs-writer && sync")
    client_sum = client.succeed(f"sha256sum {STREAM_FILE}").split()[0]
    m = NODES[new_node]
    path = nfs_host_path(m)
    assert path, f"{NFS_VOL} not visible on {new_node}"
    host_sum = m.succeed(f"sha256sum {path}/stream.txt").split()[0]
    assert host_sum == client_sum, f"checksum mismatch: client {client_sum}, volume {host_sum}"

with subtest("Prometheus reports the dead node down and the rest up"):
    def dead_seen():
        got = targets()
        return (got.get(f"expanse-{nfs_node}/{nfs_node}") == "down"
                and got.get(f"node-exporter/{nfs_node}") == "down"
                and all(got.get(f"expanse-{n}/{n}") == "up" for n in NODES if n != nfs_node))
    until(dead_seen, f"{nfs_node} down in Prometheus", 300)
    print(f"DONE: {NODE_COUNT} nodes, {nfs_node} killed, export on {new_node}, prom on {holder('prom')}")
