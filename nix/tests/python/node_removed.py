"""node-removed: the removed node's agent learns it was removed and stops its workloads.

Spliced after cluster-common.py and block-common.py.
"""

PORT = 18083
REMOVED = n3


def removed_units(m):
    return m.execute("systemctl list-units --state=active --no-legend 'expanse-block@*' 2>&1")[1].strip()


def agent_active(m):
    return m.execute("systemctl is-active expansed.service")[1].strip() in ("active", "activating", "deactivating")


def report(m):
    print(f"[{m.name}] expansed: {m.execute('systemctl is-active expansed.service')[1].strip()}")
    print(f"[{m.name}] status:\n{status(m)}")
    print(f"[{m.name}] blocks: {removed_units(m)!r}")
    print(f"[{m.name}] local revocation: {m.execute(f'expanse ctl kv list /cluster/revoked/ --stale {SOCK} 2>&1')[1]!r}")
    print(f"[{m.name}] journal:\n{m.execute('journalctl -u expansed.service -n 60 --no-pager 2>&1')[1]}")


def wait_until(predicate, what, timeout):
    deadline = time.time() + timeout
    while time.time() < deadline:
        if predicate():
            return
        time.sleep(2)
    report(REMOVED)
    raise AssertionError(f"timed out waiting for {what}")


form("rm")

with subtest("a replica of the block runs on every node"):
    deploy(n1, "web", echo_yaml("web", 3, PORT, "rm\n", antiaffinity=True), port=PORT)
    wait_phase(n1, "web", ["RUNNING"], 120)
    wait_until(lambda: unit_running(REMOVED, "default", "web", 0) or removed_units(REMOVED) != "",
               f"a block unit on {REMOVED.name}", 60)

with subtest("the removed node's leadership moves away first"):
    if leader_of(status(n1)) == REMOVED.name:
        n1.succeed(f"expanse ctl node transfer-leadership n1 {SOCK}")
        wait_until(lambda: leader_of(status(n1)) not in ("", REMOVED.name), "leadership to move", 60)

with subtest("removing the node"):
    n1.succeed(f"expanse ctl node remove {REMOVED.name} {SOCK}")
    wait_quorum("2/2", 60)
    began = time.time()

with subtest("the removed node stops its block replica"):
    wait_until(lambda: removed_units(REMOVED) == "", f"{REMOVED.name} to stop its block units", 120)
    print(f"block units stopped {time.time() - began:.0f}s after the removal")

with subtest("the removed node's agent stops for good and says why"):
    wait_until(lambda: not agent_active(REMOVED), f"{REMOVED.name}'s agent to stop", 60)
    time.sleep(15)  # longer than RestartSec: systemd must not bring it back
    if agent_active(REMOVED):
        report(REMOVED)
        raise AssertionError(f"{REMOVED.name}'s agent was restarted after its removal")
    journal = REMOVED.succeed("journalctl -u expansed.service --no-pager 2>&1")
    if "removed from the cluster" not in journal:
        report(REMOVED)
        raise AssertionError("the journal does not say the node was removed")

with subtest("the remaining nodes keep serving"):
    rc, out = kv(n1, "put /rm/after ok")
    if rc != 0:
        raise AssertionError(out)
