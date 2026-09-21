"""vol-resync-incremental (X4): a replica that missed writes catches up by bitmap, not by full copy.

A replication-3 volume is filled, one secondary's link is cut, and a small region is overwritten
on the primary. When the link returns, the bytes the returning node receives over the mesh must be
close to what was overwritten (far below the volume size) and the resync must finish inside the
budget, with every replica byte-identical. A control then forces a full resync on the same node: it
must move about the whole volume, which shows the counter can tell the two apart.

Runs after cluster-common.py and vol_cluster.py.
"""

SIZE_MIB = 1024
DRIFT_MIB = 100
DRIFT_AT_MIB = 400
NAME = "vrsi"
MAX_BYTES = 5 * DRIFT_MIB * 1024 * 1024 // 2  # 2.5x the drift; a full copy is 4x this
MAX_SECONDS = 60
FULL_COPY_MIN = 9 * SIZE_MIB * 1024 * 1024 // 10


def rx_bytes(m):
    """Bytes received on the mesh interface, which carries all DRBD traffic."""
    for line in m.succeed("cat /proc/net/dev").splitlines():
        if line.strip().startswith("exp0:"):
            return int(line.split(":", 1)[1].split()[0])
    raise Exception(f"no exp0 on {m.name}")


def resync_measured(primary, victim, res, trigger):
    """Run trigger, then return (bytes the victim received, seconds) until every replica is UpToDate."""
    before, start = rx_bytes(victim), time.time()
    trigger()
    wait_for(lambda: fully_replicated(primary, res), "every replica to be UpToDate", 300)
    return rx_bytes(victim) - before, time.time() - start


def invalidate(primary, victim, res):
    victim.succeed(f"drbdadm invalidate {res}")
    wait_for(lambda: not fully_replicated(primary, res), "the invalidation to reach the primary", 30)


def dump_on_failure(res, machines):
    for m in machines:
        print(f"[{m.name}] drbd:\n{m.execute(f'drbdsetup status {res} --verbose --statistics 2>&1')[1]}")


form("vrsi")

with subtest("a replication-3 volume is created and filled"):
    n1.succeed(f"expanse ctl volume create {NAME} --size {SIZE_MIB}Mi --replication 3")
    wait_for(lambda: (volume_row(n1, NAME) or {}).get("state") == "healthy", "the volume to be Healthy", 120)
    res = volume_row(n1, NAME)["id"]
    wait_for(lambda: len(primaries(res)) == 1, "one primary")
    primary = primaries(res)[0]
    victim = [m for m in NODES if m is not primary][0]
    dev = device_of(primary)
    fill_paced(primary, dev, SIZE_MIB)
    wait_for(lambda: fully_replicated(primary, res), "the replicas to be UpToDate")
    print(f"{primary.name} is primary, {victim.name} will miss {DRIFT_MIB} MiB of writes")

with subtest("a secondary is cut off and the primary overwrites a region"):
    victim.block()
    wait_for(lambda: connection_of(primary, res, victim) != "Connected", f"{victim.name} to be seen as gone", 120)
    primary.succeed(f"dd if=/dev/urandom of={dev} bs=1M seek={DRIFT_AT_MIB} count={DRIFT_MIB} oflag=direct conv=notrunc,fsync")
    want = checksum(primary, dev, SIZE_MIB)

with subtest("the returning node receives about what it missed, in time"):
    try:
        received, seconds = resync_measured(primary, victim, res, victim.unblock)
    except Exception:
        dump_on_failure(res, [primary, victim])
        raise
    mib = received / 1024 / 1024
    print(f"incremental resync: {mib:.0f} MiB received in {seconds:.0f}s for {DRIFT_MIB} MiB overwritten")
    assert received < MAX_BYTES, f"received {mib:.0f} MiB for a {DRIFT_MIB} MiB drift: this is more than a bitmap resync"
    assert seconds < MAX_SECONDS, f"resync took {seconds:.0f}s, budget {MAX_SECONDS}s"

with subtest("every replica holds the same bytes"):
    for m in NODES:
        assert checksum(m, f"/dev/vg0/{res}", SIZE_MIB) == want, f"{m.name} differs from the primary"

with subtest("control: an invalidated replica is copied whole"):
    full, full_seconds = resync_measured(primary, victim, res, lambda: invalidate(primary, victim, res))
    print(f"full resync: {full / 1024 / 1024:.0f} MiB received in {full_seconds:.0f}s")
    assert full > FULL_COPY_MIN, "a full resync moved less than the volume: the byte count cannot separate the two cases"
    assert checksum(victim, f"/dev/vg0/{res}", SIZE_MIB) == want
    print(f"VOL-RESYNC-INCREMENTAL PASSED: {mib:.0f} MiB in {seconds:.0f}s incremental, {full / 1024 / 1024:.0f} MiB full")
