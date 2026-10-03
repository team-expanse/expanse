"""Cluster administration with every agent running: tokens, joins, leadership, removal.

Spliced after cluster-common.py, which provides n1..n4, form, join_and_start,
status, wait_quorum, leader_of and has_node.
"""

SOCK = "--socket /run/expanse/agent.sock"
CORE = [n1, n2, n3]


def ctl(m, args):
    return m.succeed(f"expanse ctl {args} {SOCK} 2>&1")


def leader_now():
    return leader_of(status(n1))


def a_follower(among=CORE):
    lead = leader_now()
    return next(m for m in among if m.name != lead)


def wait_until(predicate, what, timeout=60):
    deadline = time.time() + timeout
    while time.time() < deadline:
        if predicate():
            return
        time.sleep(1)
    raise AssertionError(f"timed out waiting for {what}: {status(n1)}")


def all_agents_running():
    for m in [n1, n2, n3, n4]:
        m.succeed("systemctl is-active expansed.service")


form("admin")

with subtest("a follower mints and lists a join token with its agent running"):
    f = a_follower()
    out = f.succeed(f"expanse cluster token create --uses 1 {SOCK}")
    found = re.search(r"expanse-join-[A-Za-z0-9_-]+", out)
    assert found, f"no token from {f.name}: {out}"
    token = found.group(0)
    listing = f.succeed(f"expanse cluster token list {SOCK}")
    assert f.name in listing, f"token list does not name its issuer {f.name}: {listing}"

with subtest("n4 joins with that token"):
    n4.succeed("systemctl stop expansed.service")
    join_and_start(n4, token)
    wait_quorum("4/3", 90)
    all_agents_running()

with subtest("a follower hands leadership to itself"):
    f = a_follower()
    out = ctl(f, f"node transfer-leadership {f.name}")
    assert f"leader is now {f.name}" in out, out
    wait_until(lambda: leader_now() == f.name, f"{f.name} to lead")

with subtest("removing the leader is refused and names the command to run first"):
    lead = leader_now()
    rc, out = a_follower().execute(f"expanse ctl node remove {lead} {SOCK} 2>&1")
    assert rc != 0 and "expanse ctl node transfer-leadership" in out, out

with subtest("a follower removes n4"):
    f = a_follower()
    ctl(f, "node remove n4")
    wait_quorum("3/2", 60)
    nodes = ctl(f, "node list -o json")
    assert '"n4"' not in nodes, f"n4 still listed: {nodes}"

with subtest("cluster leave removes the node it runs on"):
    if leader_now() == "n3":
        ctl(n3, "node transfer-leadership n1")
        wait_until(lambda: leader_now() not in ("", "n3"), "leadership to leave n3")
    n3.succeed(f"expanse cluster leave {SOCK}")
    wait_quorum("2/2", 60)
    assert not has_node(status(n1), "n3"), status(n1)

with subtest("the two remaining nodes still serve writes"):
    rc, out = kv(n1, "put /admin/after-leave ok")
    assert rc == 0, out
    rc, out = kv(n2, "get /admin/after-leave")
    assert rc == 0 and out.strip() == "ok", out
