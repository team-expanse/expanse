"""vol-full-restart: a replication-3 DRBD volume survives all three nodes crashing at once.

Fill the volume with random data, checksum it, hard-kill every VM together (qemu quit, no
flush), boot all three, and require the volume to come back with one Primary, every replica
UpToDate and Healthy in the controller within the budget, and the data byte-identical on the
DRBD device and on every replica's backing LV. This is a cold cluster start: raft, the
controller, DRBD and the agents all recover from disk with no survivor to lean on.

Runs after cluster-common.py and vol_cluster.py.
"""

SIZE_MIB = 512
HEALTHY_BUDGET_S = 60  # from every agent being up, not from power-on
VG = "vg0"


def controller_healthy(m):
    rc, out = m.execute("expanse ctl volume list 2>&1")
    return rc == 0 and "healthy" in out.lower()


def recovered():
    return len(primaries(res)) == 1 and all(fully_replicated(m, res) for m in NODES) and controller_healthy(n1)


form("volfull")

with subtest("volume created, replicated and filled"):
    n1.succeed(f"expanse ctl volume create vfr --size {SIZE_MIB}Mi --replication 3")
    for m in NODES:
        m.wait_until_succeeds("drbdadm status | grep -q '^vol-'", timeout=180)
    res = n1.succeed("drbdadm status | head -1 | cut -d' ' -f1").strip()
    wait_for(lambda: all(fully_replicated(m, res) for m in NODES), "every replica UpToDate", 300)
    wait_for(lambda: len(primaries(res)) == 1, "one primary")
    primary = primaries(res)[0]
    fill_paced(primary, device_of(primary), SIZE_MIB)
    ref = checksum(primary, device_of(primary), SIZE_MIB)
    print(f"reference checksum: {ref}")

with subtest("hard-kill all three nodes together"):
    for m in NODES:
        m.crash()

with subtest("cold start: one Primary, every replica UpToDate, Healthy within budget"):
    for m in NODES:
        m.start()
    for m in NODES:
        m.wait_for_unit("multi-user.target", timeout=180)
        m.wait_for_unit("expansed.service", timeout=120)
        wait_agent_ready(m)
    started = time.time()
    try:
        wait_for(recovered, "the volume to recover", HEALTHY_BUDGET_S)
    except Exception:
        for m in NODES:
            print(f"[{m.name}] drbd:\n{drbd_status(m, res)}")
            print(f"[{m.name}] agent:\n{m.execute('journalctl -u expansed.service -n 20 --no-pager 2>&1')[1]}")
        print(n1.execute("expanse ctl volume list 2>&1")[1])
        raise
    elapsed = time.time() - started
    print(f"recovered {elapsed:.0f}s after every agent was up")

with subtest("the data is intact on the DRBD device and on every replica's LV"):
    primary = primaries(res)[0]
    assert checksum(primary, device_of(primary), SIZE_MIB) == ref, "DRBD device does not hold the pre-crash data"
    for m in NODES:
        assert checksum(m, f"/dev/{VG}/{res}", SIZE_MIB) == ref, f"{m.name}'s replica differs from the pre-crash data"
    print(f"VOL-FULL-RESTART PASSED: recovered in {elapsed:.0f}s, all replicas match the pre-crash checksum")
