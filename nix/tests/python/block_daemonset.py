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

with subtest("cordon a node from a follower, agents running"):
    # Lifecycle commands go through the local agent, which forwards to the leader.
    rc, out = n1.execute(f"expanse cluster status {SOCK}")
    leader_ip = out.split("leader:")[1].split(":")[0].strip()
    by_ip = {"192.168.1.1": n1, "192.168.1.2": n2, "192.168.1.3": n3, "192.168.1.4": n4}
    followers = [m for ip, m in by_ip.items() if ip != leader_ip]
    followers[0].succeed("expanse ctl node cordon n2")
    out = followers[1].succeed("expanse ctl node list -o json")
    cordoned = {n["id"]: n.get("cordoned", False) for n in json.loads(out)["nodes"]}
    assert cordoned == {"n1": False, "n2": True, "n3": False, "n4": False}, cordoned


with subtest("the daemonset keeps its replica on the cordoned node"):
    time.sleep(12)  # two controller periods to act on the cordon
    b = get_json(n1, "ds")
    running = [p for p in (b or {}).get("status", {}).get("placements", [])
               if p.get("phase") == "RUNNING"]
    assert sorted(p.get("nodeId") for p in running) == ["n1", "n2", "n3", "n4"], b.get("status")
    echo_responds(n2, 18086, "ds\n", node="localhost")


with subtest("uncordon from another node"):
    followers[1].succeed("expanse ctl node uncordon n2")
    out = followers[0].succeed("expanse ctl node list -o json")
    assert not any(n.get("cordoned") for n in json.loads(out)["nodes"]), out
