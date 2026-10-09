"""vol-verify (C4b): `volume verify` finds a replica that differs and `volume resync` repairs it.

Three replicas hold random data. A first verify finds nothing. One secondary's backing device is then
overwritten behind DRBD's back (its resource is down while it happens, as a bad disk would look), and
a second verify must blame exactly that replica: the kernel counts the differing blocks against it and
against no other, and its contents differ from the other two.

A control shows why DRBD's own advice is not enough: disconnecting and reconnecting after a verify
forgets what it found, so the replica still differs. `volume resync --node` then rebuilds it from its
peers, all three replicas are identical again, and a last verify is clean.

`volume inspect` (C4d) must show all of this without anyone reading the kernel: a replica being verified,
the blocks a verify found against it, and that count going away once the kernel's does. The control reads
what it shows after a reconnect from the kernel's own count rather than assuming one.

Runs after cluster-common.py and vol_cluster.py.
"""

import json

SIZE_MIB = 256
NAME = "vv"
DAMAGE_AT_MIB = 100
DAMAGE_MIB = 2
BLOCKS_4K = DAMAGE_MIB * 256
# Two controller ticks (5s) plus an agent report (2s): a stale report cannot outlast it.
SETTLE_S = 12


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


def inspect_rows(m):
    """`volume inspect` as {replica: {"sync": ..., "oos": ...}} (the SYNC and OUT OF SYNC columns)."""
    rows = {}
    for line in m.execute(f"expanse ctl volume inspect {NAME} 2>&1")[1].splitlines():
        cols = line.split()
        if len(cols) == 6 and cols[0] in {x.name for x in NODES}:
            rows[cols[0]] = {"sync": cols[3], "oos": cols[4]}
    return rows


def shown_out_of_sync(m, node):
    return inspect_rows(m).get(node, {}).get("oos")


def as_inspect_shows(kib):
    return "-" if kib == 0 else {DAMAGE_MIB * 1024: f"{DAMAGE_MIB}Mi"}[kib]


def sums(res):
    return {m.name: checksum(m, f"/dev/vg0/{res}", SIZE_MIB) for m in NODES}


def run_verify(asked_on, primary, res):
    """Ask for a verify from asked_on; it is done when the primary's kernel has finished it against both peers.
    Returns whether `volume inspect` showed a replica being verified along the way."""
    before = verifies_done(primary)
    asked_on.succeed(f"expanse ctl volume verify {NAME}")
    shown = []

    def finished():
        shown.append(any(r["sync"] == "verifying" for r in inspect_rows(primary).values()))
        return verifies_done(primary) >= before + 2

    wait_for(finished, "the verify to finish against both peers", 180)
    return any(shown)


def in_sync_and_healthy(primary, res):
    return fully_replicated(primary, res) and state_of(n1) == "healthy"


def reconnect(m, res):
    """Drop every connection and reconnect; the agent may reconnect a peer first, which is as good."""
    m.succeed(f"drbdadm disconnect {res}")
    rc, out = m.execute(f"drbdadm connect {res} 2>&1")
    assert rc == 0 or "Device has a net-config" in out, out


def healthy_for(primary, res, seconds):
    """A predicate true once the volume has stayed in sync and Healthy for `seconds`,
    long enough for the controller to act on any report taken while it was not."""
    since = []

    def check():
        if not in_sync_and_healthy(primary, res):
            since.clear()
            return False
        since[:] = since or [time.time()]
        return time.time() - since[0] >= seconds

    return check


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
        assert run_verify(good, primary, res), "inspect never showed the verify running"
        assert not kernel_lines(primary, "Online verify found"), "a clean volume was reported as damaged"
        assert set(out_of_sync_kib(primary, res).values()) == {0}, out_of_sync_kib(primary, res)
        wait_for(lambda: {r["oos"] for r in inspect_rows(n1).values()} == {"-"}, "inspect to show nothing out of sync", 30)

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

    with subtest("inspect names the damaged replica and the amount, from any node"):
        for m in NODES:
            wait_for(lambda m=m: shown_out_of_sync(m, bad.name) == f"{DAMAGE_MIB}Mi", f"{m.name}'s inspect to blame {bad.name}", 30)
            assert shown_out_of_sync(m, good.name) == "-", inspect_rows(m)
            assert shown_out_of_sync(m, primary.name) == "-", inspect_rows(m)

    with subtest("control: reconnecting after a verify does not repair it"):
        reconnect(primary, res)
        wait_for(healthy_for(primary, res, SETTLE_S), "the volume to reconnect and stay Healthy", 120)
        still = sums(res)
        assert still[bad.name] != still[primary.name] == still[good.name], f"a reconnect changed the replicas: {still}"
        kib = out_of_sync_kib(primary, res)[bad.name]
        print(f"after the reconnect the kernel counts {kib} KiB against {bad.name} though its data still differs")
        wait_for(lambda: shown_out_of_sync(n1, bad.name) == as_inspect_shows(kib), "inspect to follow the kernel's count", 30)

    with subtest("resync rebuilds the damaged replica from its peers"):
        good.succeed(f"expanse ctl volume resync {NAME} --node {bad.name}")
        wait_for(lambda: len(set(sums(res).values())) == 1, "the replicas to be identical", 180)
        wait_for(lambda: in_sync_and_healthy(primary, res), "the volume to be Healthy again", 120)

    with subtest("a last verify is clean"):
        found_before = blamed(primary, bad)
        run_verify(good, primary, res)
        assert blamed(primary, bad) == found_before, "the repaired replica still differs"
        assert set(out_of_sync_kib(primary, res).values()) == {0}, out_of_sync_kib(primary, res)
        wait_for(lambda: {r["oos"] for r in inspect_rows(n1).values()} == {"-"}, "inspect to show nothing out of sync", 30)
        print("VOL-VERIFY PASSED")
except Exception:
    dump_on_failure(res)
    raise
