"""NODE_COUNT nodes: does everything still work at this size?

Forms the cluster, then checks the mesh, block spread, raft reads and writes from every node,
a partition with the leader on the minority side, drain and removal of one node, and a
restart of every agent at once. Correctness, not speed: timeouts are generous.
Runs after cluster-common.py, block-common.py and client-common.py; expects NODE_COUNT
spliced in.
"""
import functools

print = functools.partial(print, flush=True)  # stdout is a file; show progress as it happens

NODES = {f"n{i}": globals()[f"n{i}"] for i in range(1, NODE_COUNT + 1)}
MAJORITY = NODE_COUNT // 2 + 1
# The leader goes to the minority side so the majority must elect a new one.
MAJOR = list(NODES)[:MAJORITY]
MINOR = list(NODES)[MAJORITY:]
DS_PORT, WEB_PORT = 18086, 18087


def until(predicate, what, timeout, every=2):
    deadline = time.time() + timeout
    while time.time() < deadline:
        if predicate():
            return
        time.sleep(every)
    raise AssertionError(f"timed out after {timeout}s waiting for {what}")


def quorum_of(m):
    found = re.search(r"quorum:\s+(\d+)/(\d+)", status(m))
    return (int(found.group(1)), int(found.group(2))) if found else None


def kv_get(m, key):
    rc, out = kv(m, f"get {key}")
    return out.strip() if rc == 0 else None


def kv_put_retry(m, key, value, timeout=60):
    """A write may land mid-election; retry until it is acked."""
    out = ""
    deadline = time.time() + timeout
    while time.time() < deadline:
        rc, out = kv(m, f"put {key} {value}")
        if rc == 0:
            return
        time.sleep(2)
    raise AssertionError(f"{m.name} could not write {key}: {out}")


def wait_kv_everywhere(names, key, want, timeout=60):
    for name in names:
        until(lambda m=NODES[name]: kv_get(m, key) == want, f"{name} to read {key}={want}", timeout)


def wait_block_on(name, want, timeout):
    until(lambda: placement_nodes(get_json(n1, name) or {}) == want,
          f"{name} on exactly {sorted(want)}", timeout)


def serves(m, port, body):
    rc, out = m.execute(f"curl -sS --max-time 3 http://localhost:{port}/")
    return rc == 0 and body in out


def web_nodes():
    return placement_nodes(get_json(n1, "web") or {})


def isolate(names, others):
    """Drops all LAN traffic between each node in names and every node in others."""
    for name in names:
        rules = "; ".join(
            f"nft add rule ip scale input ip saddr {addr(NODES[o])} drop; "
            f"nft add rule ip scale output ip daddr {addr(NODES[o])} drop"
            for o in others)
        NODES[name].succeed(
            "nft add table ip scale; "
            "nft add chain ip scale input '{ type filter hook input priority 0; }'; "
            "nft add chain ip scale output '{ type filter hook output priority 0; }'; " + rules)


def heal(names):
    for name in names:
        NODES[name].succeed("nft delete table ip scale")


with subtest(f"boot {NODE_COUNT} nodes"):
    start_all()
    for m in NODES.values():
        m.wait_for_unit("multi-user.target")
        m.succeed("systemctl stop expansed.service")
        addr(m)  # cache each address while the LAN is whole

with subtest(f"form a {NODE_COUNT}-node cluster that every node agrees on"):
    t0 = time.time()
    n1.succeed("expanse cluster init --data-dir /persist/expanse --name scale --node-id n1 "
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
    for name, m in NODES.items():
        until(lambda m=m: quorum_of(m) == (NODE_COUNT, MAJORITY), f"{name} to report quorum {NODE_COUNT}/{MAJORITY}", 300)
    rep = status(n1)
    assert len(leaders(rep)) == 1, f"not exactly one leader: {rep}"
    for name in NODES:
        assert has_node(rep, name), f"{name} missing from the status: {rep}"
    print(f"formed in {time.time() - t0:.0f}s\n{rep}")

with subtest("every node holds a cert signed by the cluster CA"):
    for m in NODES.values():
        out = m.succeed("openssl verify -CAfile /persist/expanse/ca/ca.pem /persist/expanse/tls/node-cert.pem")
        assert ": OK" in out, f"{m.name} cert not CA-signed: {out}"

with subtest(f"full WireGuard mesh: {NODE_COUNT - 1} peers on every node, every pair pings"):
    for m in NODES.values():
        wait_wg_peers(m, NODE_COUNT - 1, 180)
    ping_matrix(list(NODES.values()))
    ping_matrix(list(NODES.values()), ("df", DF_SIZE))

with subtest("a daemonset lands on every node; an anti-affine block on distinct nodes"):
    deploy(n1, "ds", echo_yaml("ds", None, DS_PORT, "ds\n", antiaffinity=False, strategy="DAEMONSET"), port=DS_PORT)
    deploy(n1, "web", echo_yaml("web", 3, WEB_PORT, "web\n"), port=WEB_PORT)
    wait_block_on("ds", set(NODES), 300)
    wait_placement_nodes(n1, "web", 3, 180)
    wait_phase(n1, "ds", ["RUNNING"], 300)
    wait_phase(n1, "web", ["RUNNING"], 180)
    for m in NODES.values():
        until(lambda m=m: serves(m, DS_PORT, "ds\n"), f"ds to serve on {m.name}", 120)

with subtest("a write from every node is read back on every node"):
    for name, m in NODES.items():
        kv_put_retry(m, f"/scale/{name}", f"from-{name}")
    for name in NODES:
        wait_kv_everywhere(NODES, f"/scale/{name}", f"from-{name}")

with subtest(f"partition {len(MAJOR)}|{len(MINOR)} with the leader in the minority"):
    target = MINOR[-1]
    n1.succeed(f"expanse ctl node transfer-leadership {target} {SOCK}")
    until(lambda: leader_of(status(n1)) == target, f"leadership on {target}", 60)
    isolate(MINOR, MAJOR)
    for name in MINOR:
        until(lambda m=NODES[name]: "DEGRADED" in status(m).upper(), f"{name} to report DEGRADED", 120)
    until(lambda: leader_of(status(n1)) in MAJOR, "a leader on the majority side", 120)
    kv_put_retry(n1, "/scale/partition", "majority")
    wait_kv_everywhere(MAJOR, "/scale/partition", "majority")
    for name in MINOR:
        rc, out = kv(NODES[name], "put /scale/minority nope")
        assert rc != 0, f"{name} accepted a write without quorum: {out}"
    print(status(n1))

with subtest("after healing, the minority catches up and the cluster is whole"):
    heal(MINOR)
    for name, m in NODES.items():
        until(lambda m=m: "DEGRADED" not in status(m).upper() and quorum_of(m) == (NODE_COUNT, MAJORITY),
              f"{name} to rejoin the full quorum", 180)
    assert len(leaders(status(n1))) == 1, status(n1)
    wait_kv_everywhere(NODES, "/scale/partition", "majority")
    for name, m in NODES.items():
        assert kv_get(m, "/scale/minority") is None, f"{name} holds the rejected minority write"

with subtest("draining a node moves the block off it and stops its daemonset replica"):
    gone = max(web_nodes() - {"n1", leader_of(status(n1))}, key=lambda n: int(n[1:]))
    n1.succeed(f"expanse ctl node drain {gone} {SOCK}")
    wait_block_on("ds", set(NODES) - {gone}, 180)
    until(lambda: gone not in web_nodes() and len(web_nodes()) == 3,
          f"web to move off {gone}", 180)
    wait_phase(n1, "web", ["RUNNING"], 180)
    NODES[gone].wait_until_fails(f"curl -sS --max-time 3 http://localhost:{DS_PORT}/", timeout=120)

with subtest("removing the drained node shrinks raft and the mesh"):
    if leader_of(status(n1)) == gone:
        n1.succeed(f"expanse ctl node transfer-leadership n1 {SOCK}")
    n1.succeed(f"expanse ctl node remove {gone} {SOCK}")
    REST = {name: m for name, m in NODES.items() if name != gone}
    after = (NODE_COUNT - 1, (NODE_COUNT - 1) // 2 + 1)
    for name, m in REST.items():
        until(lambda m=m: quorum_of(m) == after, f"{name} to report quorum {after[0]}/{after[1]}", 120)
    until(lambda: NODES[gone].execute("systemctl is-active expansed.service")[1].strip() not in ("active", "activating"),
          f"{gone}'s agent to stop", 180)
    for m in REST.values():
        wait_wg_peers(m, len(REST) - 1, 180)
    ping_matrix(list(REST.values()))

with subtest("every remaining agent restarted at once: one leader, state intact"):
    for m in REST.values():
        m.succeed("systemctl stop expansed.service")
    for m in REST.values():
        m.succeed("systemctl start expansed.service")
    for name, m in REST.items():
        until(lambda m=m: quorum_of(m) == after, f"{name} back in quorum after the restart", 300)
    until(lambda: len(leaders(status(n1))) == 1, "a single leader after the restart", 120)
    for name in NODES:
        wait_kv_everywhere(REST, f"/scale/{name}", f"from-{name}", 120)
    wait_block_on("ds", set(REST), 300)
    wait_phase(n1, "web", ["RUNNING"], 300)
    for m in REST.values():
        until(lambda m=m: serves(m, DS_PORT, "ds\n"), f"ds to serve on {m.name}", 180)
    print(status(n1))

print("CLUSTER-SCALE COMPLETED")
