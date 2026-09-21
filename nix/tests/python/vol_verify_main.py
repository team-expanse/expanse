"""vol-verify (C4b): `volume verify` finds a replica that differs and `volume resync` repairs it.

Three replicas hold random data. A first verify finds nothing. One secondary's backing device is then
overwritten behind DRBD's back (its resource is down while it happens, as a bad disk would look), and
a second verify must blame exactly that replica: the kernel counts the differing blocks against it and
against no other, and its contents differ from the other two.

A control shows why DRBD's own advice is not enough: disconnecting and reconnecting after a verify
forgets what it found, so the replica still differs. `volume resync --node` then rebuilds it from its
peers, all three replicas are identical again, and a last verify is clean.

Runs after cluster-common.py and vol_cluster.py.
"""

import json

SIZE_MIB = 256
NAME = "vv"
DAMAGE_AT_MIB = 100
DAMAGE_MIB = 2
BLOCKS_4K = DAMAGE_MIB * 256


def state_of(m):
    return ((volume_row(m, NAME) or {}).get("state") or "").replace("_", "")


def kernel_lines(m, pattern):
    return m.execute(f"dmesg | grep -E '{pattern}'")[1].splitlines()


def verifies_done(m):
    return len(kernel_lines(m, "Online verify done"))


def blamed(m, peer):
    """How many times m's kernel has reported that its data and peer's differ."""
    return len([line for line in kernel_lines(m, "Online verify found") if f" {peer.name}: " in line])


def out_of_sync_kib(m, res):
    status = json.loads(m.succeed(f"drbdsetup status {res} --json --statistics"))[0]
    return {c["name"]: sum(v["out-of-sync"] for v in c["peer_devices"]) for c in status["connections"]}


def sums(res):
    return {m.name: checksum(m, f"/dev/vg0/{res}", SIZE_MIB) for m in NODES}


def run_verify(asked_on, primary, res):
    """Ask for a verify from asked_on; it is done when the primary's kernel has finished it against both peers."""
    before = verifies_done(primary)
    asked_on.succeed(f"expanse ctl volume verify {NAME}")
    wait_for(lambda: verifies_done(primary) >= before + 2, "the verify to finish against both peers", 180)


def in_sync_and_healthy(primary, res):
    return fully_replicated(primary, res) and state_of(n1) == "healthy"


def dump_on_failure(res):
    for m in NODES:
        print(f"[{m.name}] drbd:\n{drbd_status(m, res)}")
        print(f"[{m.name}] volume:\n{m.execute('expanse ctl volume list 2>&1')[1]}")
        print(f"[{m.name}] kernel:\n{m.execute('dmesg | grep -i drbd | tail -20')[1]}")
        print(f"[{m.name}] agent:\n{m.execute('journalctl -u expansed.service -n 20 --no-pager 2>&1')[1]}")


form("vv")

with subtest("a replication-3 volume is Healthy and holds random data"):
    n1.succeed(f"expanse ctl volume create {NAME} --size {SIZE_MIB}Mi --replication 3")
    wait_for(lambda: state_of(n1) == "healthy", "the volume to be Healthy", 120)
    res = volume_row(n1, NAME)["id"]
    wait_for(lambda: len(primaries(res)) == 1, "one primary")
    primary = primaries(res)[0]
    bad, good = [m for m in NODES if m is not primary]
    fill_paced(primary, device_of(primary), SIZE_MIB)
    wait_for(lambda: in_sync_and_healthy(primary, res), "every replica UpToDate")
    assert len(set(sums(res).values())) == 1, f"the replicas differ before anything was damaged: {sums(res)}"

try:
    with subtest("a verify of intact replicas finds nothing"):
        run_verify(good, primary, res)
        assert not kernel_lines(primary, "Online verify found"), "a clean volume was reported as damaged"
        assert set(out_of_sync_kib(primary, res).values()) == {0}, out_of_sync_kib(primary, res)

    with subtest("damage one secondary's disk behind DRBD's back"):
        bad.succeed(
            f"drbdadm down {res} && dd if=/dev/urandom of=/dev/vg0/{res} bs=1M seek={DAMAGE_AT_MIB} "
            f"count={DAMAGE_MIB} oflag=direct conv=notrunc,fsync"
        )
        wait_for(lambda: in_sync_and_healthy(primary, res), "the damaged replica to reconnect as UpToDate", 120)
        found = sums(res)
        assert found[bad.name] != found[primary.name] == found[good.name], f"the damage is not there: {found}"

    with subtest("verify blames the damaged replica and no other"):
        run_verify(good, primary, res)
        assert blamed(primary, bad) == 1, kernel_lines(primary, "Online verify")
        assert blamed(primary, good) == 0, kernel_lines(primary, "Online verify")
        assert f"{BLOCKS_4K} 4k blocks" in kernel_lines(primary, "Online verify found")[0], kernel_lines(primary, "Online verify found")
        assert out_of_sync_kib(primary, res) == {bad.name: DAMAGE_MIB * 1024, good.name: 0}, out_of_sync_kib(primary, res)

    with subtest("control: reconnecting after a verify does not repair it"):
        primary.succeed(f"drbdadm disconnect {res} && drbdadm connect {res}")
        wait_for(lambda: in_sync_and_healthy(primary, res), "the volume to reconnect", 120)
        still = sums(res)
        assert still[bad.name] != still[primary.name] == still[good.name], f"a reconnect changed the replicas: {still}"

    with subtest("resync rebuilds the damaged replica from its peers"):
        good.succeed(f"expanse ctl volume resync {NAME} --node {bad.name}")
        wait_for(lambda: len(set(sums(res).values())) == 1, "the replicas to be identical", 180)
        wait_for(lambda: in_sync_and_healthy(primary, res), "the volume to be Healthy again", 120)

    with subtest("a last verify is clean"):
        found_before = blamed(primary, bad)
        run_verify(good, primary, res)
        assert blamed(primary, bad) == found_before, "the repaired replica still differs"
        assert set(out_of_sync_kib(primary, res).values()) == {0}, out_of_sync_kib(primary, res)
        print("VOL-VERIFY PASSED")
except Exception:
    dump_on_failure(res)
    raise
