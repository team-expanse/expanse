# 4-token budget: n2/n3 consume two; the remaining one (persisted at
# /root/join-token on n1) is for n4's late join.
form("test", "voter", 4)

with subtest("deploy daemonset, exactly 1 per node"):
    # V6: no replicas key. One replica per node → a fixed port is
    # conflict-free, and lets us curl each replica directly.
    deploy(n1, "ds", echo_yaml("ds", None, 18086, "ds\n",
                               antiaffinity=False, strategy="DAEMONSET"))
    # A node whose agent has only just started may not be Ready yet, so wait for all three.
    b = wait_placement_nodes(n1, "ds", 3, 60)
    wait_phase(n1, "ds", ["RUNNING"], 30)
    nodes = placement_nodes(b)

with subtest("each replica serves on its own node"):
    for node in nodes:
        m = {"n1": n1, "n2": n2, "n3": n3}[node]
        echo_responds(m, 18086, "ds\n", node="localhost")

with subtest("new node gets a replica automatically"):
    n4.wait_for_unit("multi-user.target")
    n4.succeed("systemctl stop expansed.service")
    rc, token = n2.execute("cat /root/join-token")
    join_and_start(n4, token.strip(), "voter")
    b = wait_placement_nodes(n1, "ds", 4, 60)
    nodes = placement_nodes(b)
    assert "n4" in nodes, f"daemonset never extended to n4: {b.get('status')}"
    echo_responds(n4, 18086, "ds\n", node="localhost")

MACHINES = {"n1": n1, "n2": n2, "n3": n3, "n4": n4}


def wait_nodes(name, want, timeout):
    """Poll until the block's live placements sit on exactly the want set."""
    deadline = time.time() + timeout
    got = None
    while time.time() < deadline:
        got = placement_nodes(get_json(n1, name) or {})
        if got == want:
            return
        time.sleep(2)
    raise AssertionError(f"{name} on {sorted(got or [])}, want {sorted(want)}")


def node_list(m):
    return {n["id"]: n for n in json.loads(m.succeed("expanse ctl node list -o json"))["nodes"]}


with subtest("deploy a 3-replica block across the 4 nodes"):
    deploy(n1, "web", echo_yaml("web", 3, 18087, "web\n"))
    b = wait_placement_nodes(n1, "web", 3, 90)
    wait_phase(n1, "web", ["RUNNING"], 60)
    web_nodes = placement_nodes(b)
    target = min(web_nodes)
    spare = ({"n1", "n2", "n3", "n4"} - web_nodes).pop()

with subtest("cordon a node from a follower, agents running"):
    # Lifecycle commands go through the local agent, which forwards to the leader.
    rc, out = n1.execute(f"expanse cluster status {SOCK}")
    leader_ip = out.split("leader:")[1].split(":")[0].strip()
    by_ip = {"192.168.1.1": n1, "192.168.1.2": n2, "192.168.1.3": n3, "192.168.1.4": n4}
    followers = [m for ip, m in by_ip.items() if ip != leader_ip]
    followers[0].succeed(f"expanse ctl node cordon {target}")
    cordoned = {i: n.get("cordoned", False) for i, n in node_list(followers[1]).items()}
    assert cordoned == {i: i == target for i in MACHINES}, cordoned

with subtest("cordon keeps every replica past the unreachable grace"):
    time.sleep(45)  # the 30 s grace plus controller periods
    wait_nodes("ds", {"n1", "n2", "n3", "n4"}, 1)
    wait_nodes("web", web_nodes, 1)
    echo_responds(MACHINES[target], 18087, "web\n", node="localhost")
    echo_responds(MACHINES[target], 18086, "ds\n", node="localhost")

with subtest("drain moves the block's replica and stops the daemonset's"):
    followers[0].succeed(f"expanse ctl node drain {target}")
    assert node_list(followers[1])[target].get("draining"), "drain not reported"
    wait_nodes("web", (web_nodes - {target}) | {spare}, 60)
    wait_nodes("ds", {"n1", "n2", "n3", "n4"} - {target}, 60)
    wait_phase(n1, "web", ["RUNNING"], 60)
    MACHINES[target].wait_until_fails("curl -sS --max-time 3 http://localhost:18087/", timeout=60)
    MACHINES[target].wait_until_fails("curl -sS --max-time 3 http://localhost:18086/", timeout=60)

with subtest("uncordon ends the drain and the daemonset returns"):
    followers[1].succeed(f"expanse ctl node uncordon {target}")
    nodes = node_list(followers[0])
    assert not any(n.get("cordoned") or n.get("draining") for n in nodes.values()), nodes
    wait_nodes("ds", {"n1", "n2", "n3", "n4"}, 60)
