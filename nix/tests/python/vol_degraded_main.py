"""vol-degraded (C5): a replica whose node stays gone is rebuilt on a spare node.

A replication-2 volume lives on two of the three nodes. The node holding the secondary is
hard-killed: the volume must report Degraded and keep taking writes. Once the node has been gone
for the configured wait, the controller retires its node-id, the survivors forget it, and the
spare node builds a replacement under a fresh id and syncs it: every replica ends UpToDate with the
data written before and during the outage. The killed node comes back, finds it no longer holds
a replica, removes it, and the volume is left as it was.

Runs after cluster-common.py and vol_cluster.py.
"""

SIZE_MIB = 256
VG = "vg0"
NAME = "vdeg"
SOCK = "/run/expanse/agent.sock"
DEGRADE_BUDGET_S = 180  # lease expiry plus skew, from the crash
REBUILD_BUDGET_S = 300  # the configured wait plus retire, forget, create and sync


def row(m):
    return volume_row(m, NAME)


def state_of(m):
    found = row(m)
    return found["state"] if found else ""


def synced_with(primary, res, peer):
    """The primary has exactly one peer, it is `peer`, and its disk is UpToDate."""
    text = drbd_status(primary, res)
    return text.count("peer-disk:") == 1 and text.count("peer-disk:UpToDate") == 1 and f"\n  {peer.name} " in text


def has_lv(m, res):
    return m.execute(f"lvs --noheadings {VG}/{res} 2>&1")[0] == 0


def dump_on_failure(res, machines):
    for m in machines:
        print(f"[{m.name}] drbd:\n{drbd_status(m, res)}")
        print(f"[{m.name}] volume:\n{m.execute('expanse ctl volume list 2>&1')[1]}")
        print(f"[{m.name}] drbdsetup:\n{m.execute(f'drbdsetup status {res} --json 2>&1')[1]}")
        print(f"[{m.name}] agent:\n{m.execute('journalctl -u expansed.service -n 25 --no-pager 2>&1')[1]}")


form("voldeg")

with subtest("a replication-2 volume is created, replicated and filled"):
    n1.succeed(f"expanse ctl volume create {NAME} --size {SIZE_MIB}Mi --replication 2")
    try:
        wait_for(lambda: state_of(n1) == "healthy", "the volume to be Healthy", 120)
    except Exception:
        dump_on_failure(row(n1)["id"] if row(n1) else "", NODES)
        raise
    found = row(n1)
    res = found["id"]
    holders = [m for m in NODES if m.name in found["nodes"]]
    spare = [m for m in NODES if m not in holders][0]
    wait_for(lambda: len(primaries(res, holders)) == 1, "one primary")
    primary = primaries(res, holders)[0]
    victim = [m for m in holders if m is not primary][0]
    dev = device_of(primary)
    fill_paced(primary, dev, SIZE_MIB)
    print(f"{primary.name} is primary, {victim.name} will be killed, {spare.name} is the spare")

with subtest("the secondary's node is hard-killed: Degraded, and the volume keeps taking writes"):
    victim.crash()
    wait_for(lambda: state_of(primary) == "degraded", "the volume to be Degraded", DEGRADE_BUDGET_S)
    primary.succeed(f"dd if=/dev/urandom of={dev} bs=1M seek={SIZE_MIB - 8} count=4 oflag=direct conv=notrunc,fsync")
    ref = checksum(primary, dev, SIZE_MIB)

with subtest("after the wait, the spare builds a replacement and syncs it"):
    try:
        wait_for(lambda: has_lv(spare, res), "the spare to get a backing LV", REBUILD_BUDGET_S)
        wait_for(lambda: synced_with(primary, res, spare), "the replacement to be UpToDate", REBUILD_BUDGET_S)
        wait_for(lambda: state_of(primary) == "healthy", "the volume to be Healthy again", 120)
    except Exception:
        dump_on_failure(res, [primary, spare])
        raise
    assert row(primary)["nodes"] == sorted([primary.name, spare.name]), row(primary)
    assert checksum(spare, f"/dev/{VG}/{res}", SIZE_MIB) == ref, "the replacement does not hold the volume's data"
    ids = primary.succeed(f"expanse ctl kv --socket {SOCK} get /drbd/vol/{res}")
    assert spare.name in ids and victim.name not in ids, f"allocation still names the lost node: {ids}"

with subtest("the returning node removes its stale replica and the volume is unchanged"):
    victim.start()
    victim.wait_for_unit("multi-user.target", timeout=180)
    victim.wait_for_unit("expansed.service", timeout=120)
    wait_agent_ready(victim)
    wait_for(lambda: not has_lv(victim, res), "the returned node to drop its replica", 180)
    assert victim.execute(f"drbdadm status {res} 2>&1")[0] != 0, "the returned node still runs the resource"
    time.sleep(10)
    assert synced_with(primary, res, spare), drbd_status(primary, res)
    assert state_of(primary) == "healthy" and row(primary)["nodes"] == sorted([primary.name, spare.name])
    assert checksum(primary, dev, SIZE_MIB) == ref, "the volume changed while the old node came back"
    print(f"VOL-DEGRADED PASSED: {victim.name} replaced by {spare.name}, data intact")
