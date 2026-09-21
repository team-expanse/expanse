"""vol-forced (C4c): a dead node no longer holds a volume's retire or delete back.

Two replication-3 volumes live on all three nodes. One node is hard-killed and stays down.

`volume retire` on a live node is refused and changes nothing. On the dead one it drops the replica
at once, without the configured wait and with no spare to take over: the survivors forget the node's
DRBD identity, keep serving the volume's data and take new writes.

A plain `volume delete` of the second volume finishes on the live nodes but the record stays, waiting
for the dead one. `delete --force` ends the wait. A request left for a volume that does not exist is swept.
When the dead node returns it removes both replicas it still holds, the retired one and the deleted one.

Runs after cluster-common.py and vol_cluster.py.
"""

SIZE_MIB = 128
VG = "vg0"
KEPT, DOOMED = "vkeep", "vdoom"
SOCK = "/run/expanse/agent.sock"
DEGRADE_BUDGET_S = 180  # lease expiry plus skew, from the crash


def state_of(m, name):
    found = volume_row(m, name)
    return found["state"] if found else ""


def has_lv(m, res):
    return m.execute(f"lvs --noheadings {VG}/{res} 2>&1")[0] == 0


def running(m, res):
    return m.execute(f"drbdadm status {res} 2>&1")[0] == 0


def peers_of(m, res):
    return drbd_status(m, res).count("peer-disk:")


def device_for(m, res):
    return m.succeed(f"drbdadm sh-dev {res}").strip()


def synced_with_one_peer(m, res):
    lines = drbd_status(m, res).split("\n")
    return len(lines) > 1 and "disk:UpToDate" in lines[1] and peers_of(m, res) == 1 and "peer-disk:UpToDate" in "".join(lines)


def kv_keys(m, prefix):
    """The keys under prefix; `kv list` prints key, revision and value per line."""
    return [line.split()[0] for line in m.succeed(f"expanse ctl kv --socket {SOCK} list {prefix}").splitlines() if line.strip()]


def allocation(m, res):
    return m.succeed(f"expanse ctl kv --socket {SOCK} get /drbd/vol/{res}")


def dump_on_failure(machines, ress):
    for m in machines:
        print(f"[{m.name}] volumes:\n{m.execute('expanse ctl volume list 2>&1')[1]}")
        for res in ress:
            print(f"[{m.name}] drbd {res}:\n{drbd_status(m, res)}")
        print(f"[{m.name}] agent:\n{m.execute('journalctl -u expansed.service -n 25 --no-pager 2>&1')[1]}")


def create(name):
    n1.succeed(f"expanse ctl volume create {name} --size {SIZE_MIB}Mi --replication 3")
    wait_for(lambda: state_of(n1, name) == "healthy", f"{name} to be Healthy", 120)
    return volume_row(n1, name)["id"]


form("vforced")
victim, alive = n3, [n1, n2]
res = {}

with subtest("two replication-3 volumes are Healthy and hold data"):
    for name in (KEPT, DOOMED):
        res[name] = create(name)
        wait_for(lambda: len(primaries(res[name])) == 1, f"{name} to have one primary")
        p = primaries(res[name])[0]
        fill_paced(p, device_for(p, res[name]), 64)
    wait_for(lambda: all(fully_replicated(primaries(res[k])[0], res[k]) for k in res), "every replica UpToDate")

try:
    with subtest("the node dies and the controller notices"):
        victim.crash()
        wait_for(lambda: state_of(n1, KEPT) == "degraded", "the volume to be Degraded", DEGRADE_BUDGET_S)

    with subtest("retire of a live node is refused and changes nothing"):
        n1.succeed(f"expanse ctl volume retire {KEPT} --node n2")
        wait_for(lambda: not kv_keys(n1, "/volumes/_ops/retire/"), "the controller to answer the request", 60)
        time.sleep(10)
        assert volume_row(n1, KEPT)["nodes"] == ["n1", "n2", "n3"], volume_row(n1, KEPT)
        assert "n2" in allocation(n1, res[KEPT])

    with subtest("retire of the dead node drops its replica at once, with no wait and no spare"):
        n1.succeed(f"expanse ctl volume retire {KEPT} --node {victim.name}")
        wait_for(lambda: volume_row(n1, KEPT)["nodes"] == ["n1", "n2"], "the dead node's row to go", 60)
        assert victim.name not in allocation(n1, res[KEPT]), allocation(n1, res[KEPT])
        wait_for(lambda: all(peers_of(m, res[KEPT]) == 1 for m in alive), "the survivors to forget the dead peer", 60)

    with subtest("the retired volume keeps its data and takes new writes"):
        wait_for(lambda: len(primaries(res[KEPT], alive)) == 1, "a live primary")
        p = primaries(res[KEPT], alive)[0]
        p.succeed(f"dd if=/dev/urandom of={device_for(p, res[KEPT])} bs=1M seek=100 count=4 oflag=direct conv=notrunc,fsync")
        wait_for(lambda: synced_with_one_peer(p, res[KEPT]), "both live replicas UpToDate", 60)
        sums = {m.name: checksum(m, f"/dev/{VG}/{res[KEPT]}", SIZE_MIB) for m in alive}
        assert len(set(sums.values())) == 1, f"the live replicas differ: {sums}"

    with subtest("a plain delete finishes on the live nodes but waits for the dead one"):
        n1.succeed(f"expanse ctl volume delete {DOOMED}")
        wait_for(lambda: not any(has_lv(m, res[DOOMED]) for m in alive), "the live nodes to remove the replica", 60)
        time.sleep(15)
        assert volume_row(n1, DOOMED) is not None, "the record was dropped without the dead node's word"

    with subtest("delete --force ends the wait, and a request for a volume that is gone is swept"):
        n1.succeed(f"expanse ctl volume delete {DOOMED} --force")
        n1.succeed(f"expanse ctl kv --socket {SOCK} put /volumes/_ops/verify/vol-nosuch '{{}}'")
        wait_for(lambda: volume_row(n1, DOOMED) is None, "the record to go", 60)
        wait_for(lambda: not kv_keys(n1, "/volumes/_ops/verify/"), "the stale request to be swept", 60)
        assert kv_keys(n1, f"/volumes/_gone/{victim.name}/") == [f"/volumes/_gone/{victim.name}/{res[DOOMED]}"], kv_keys(n1, "/volumes/_gone/")

    with subtest("the returning node removes the replicas it no longer owns"):
        victim.start()
        victim.wait_for_unit("multi-user.target", timeout=180)
        victim.wait_for_unit("expansed.service", timeout=120)
        wait_agent_ready(victim)
        for name in (KEPT, DOOMED):
            wait_for(lambda: not has_lv(victim, res[name]), f"the returned node to drop {name}", 180)
            assert not running(victim, res[name]), f"the returned node still runs {name}"
        wait_for(lambda: not kv_keys(n1, "/volumes/_gone/"), "the gone mark to be cleared", 60)
        assert volume_row(n1, KEPT)["nodes"] == ["n1", "n2"], volume_row(n1, KEPT)
        assert synced_with_one_peer(primaries(res[KEPT], alive)[0], res[KEPT])
        print("VOL-FORCED PASSED")
except Exception:
    dump_on_failure([n1, n2], list(res.values()))
    raise
