"""node-removed: a removed node's agent learns it was removed and stops its workloads.

n3 is removed while connected and sees its own revocation; n4 is removed while cut off,
never receives it, and must learn it from a peer once it is reconnected.
Spliced after cluster-common.py and block-common.py.
"""

PORT = 18083


def block_units(m):
    return m.execute("systemctl list-units --state=active --no-legend 'expanse-block@*' 2>&1")[1].strip()


def agent_active(m):
    return m.execute("systemctl is-active expansed.service")[1].strip() in ("active", "activating", "deactivating")


def report(m):
    print(f"[{m.name}] expansed: {m.execute('systemctl is-active expansed.service')[1].strip()}")
    print(f"[{m.name}] status:\n{status(m)}")
    print(f"[{m.name}] blocks: {block_units(m)!r}")
    print(f"[{m.name}] local revocation: {m.execute(f'expanse ctl kv list /cluster/revoked/ --stale {SOCK} 2>&1')[1]!r}")
    print(f"[{m.name}] journal:\n{m.execute('journalctl -u expansed.service -n 60 --no-pager 2>&1')[1]}")


def wait_until(predicate, what, timeout, m):
    deadline = time.time() + timeout
    while time.time() < deadline:
        if predicate():
            return
        time.sleep(2)
    report(m)
    raise AssertionError(f"timed out waiting for {what}")


def lead_away_from(m):
    if leader_of(status(n1)) == m.name:
        n1.succeed(f"expanse ctl node transfer-leadership n1 {SOCK}")
    wait_until(lambda: leader_of(status(n1)) not in ("", m.name), f"leadership to move off {m.name}", 60, m)


def assert_retired(m, began):
    wait_until(lambda: block_units(m) == "", f"{m.name} to stop its block units", 180, m)
    print(f"{m.name} stopped its block units {time.time() - began:.0f}s later")
    wait_until(lambda: not agent_active(m), f"{m.name}'s agent to stop", 60, m)
    time.sleep(15)  # longer than RestartSec: systemd must not bring it back
    if agent_active(m):
        report(m)
        raise AssertionError(f"{m.name}'s agent was restarted after its removal")
    if "removed from the cluster" not in m.succeed("journalctl -u expansed.service --no-pager 2>&1"):
        report(m)
        raise AssertionError(f"{m.name}'s journal does not say the node was removed")


form("rm", uses=4)
n4.wait_for_unit("multi-user.target")
n4.succeed("systemctl stop expansed.service")  # it started un-enrolled at boot
join_and_start(n4, n2.succeed("cat /root/join-token").strip())
wait_quorum("4/3", 60)

with subtest("a replica of the block runs on every node"):
    deploy(n1, "web", echo_yaml("web", 4, PORT, "rm\n", antiaffinity=True), port=PORT)
    wait_phase(n1, "web", ["RUNNING"], 180)
    for m in (n3, n4):
        wait_until(lambda m=m: block_units(m) != "", f"a block unit on {m.name}", 60, m)

with subtest("a node removed while connected stops its replica and its agent"):
    lead_away_from(n3)
    n1.succeed(f"expanse ctl node remove n3 {SOCK}")
    wait_quorum("3/2", 60)
    assert_retired(n3, time.time())

with subtest("a node removed while cut off keeps running until it is reconnected"):
    lead_away_from(n4)
    n4.block()
    try:
        wait_until(lambda: "DEGRADED" in status(n4).upper(), "n4 to lose quorum", 60, n4)
        n1.succeed(f"expanse ctl node remove n4 {SOCK}")
        wait_quorum("2/2", 60)
        # Outlast raft's one last attempt to send n4 its removal (10s transport timeout).
        time.sleep(30)
        if block_units(n4) == "":
            report(n4)
            raise AssertionError("n4 stopped its replica while cut off; the scenario needs it running")
    finally:
        n4.unblock()

with subtest("once reconnected, the node learns of its removal from a peer and stops"):
    assert_retired(n4, time.time())
    if "a peer says this node was removed" not in n4.succeed("journalctl -u expansed.service --no-pager 2>&1"):
        report(n4)
        raise AssertionError("n4 did not learn of its removal from a peer")

with subtest("the remaining nodes keep serving"):
    rc, out = kv(n1, "put /rm/after ok")
    if rc != 0:
        raise AssertionError(out)
