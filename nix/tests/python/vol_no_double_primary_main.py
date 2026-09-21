"""vol-no-double-primary (E6): through a partition storm, no two nodes are ever Primary at once.

Every node records its DRBD role changes with `drbdsetup events2`, with microsecond timestamps. A
seeded storm then cuts nodes off from the mesh and heals them again, mostly the primary itself. Each
node's clock is mapped onto the driver's, and the recorded histories must never show two nodes
Primary over the same instant. The storm must have moved the primary between nodes, or the check
proved nothing. Afterwards the volume converges: one primary, every replica UpToDate and identical.

A control then makes a real double primary (a partitioned primary whose agent is frozen cannot
demote, so the majority side promotes another node) and the same pipeline must report it. The stale
primary has lost quorum, so it must refuse writes while the new one accepts them.

Runs after cluster-common.py and vol_cluster.py; the wrapper imports vol_roles as `roles`.
"""

import random

SIZE_MIB = 256
NAME = "vndp"
SEED = 20260921
# The election moves the primary only after its node has been unreachable for the 10 s lease TTL
# plus the 15 s skew allowance, so "long" cuts force a handoff and "short" ones flap the primary.
SCHEDULE = ["long", "long", "short", "short", "pair", "pair", "other"]
CUT_S = {"long": (35, 45), "short": (4, 9), "pair": (10, 25), "other": (10, 25)}
HEAL_S = (8, 18)
TOLERANCE_S = 0.05  # what clock mapping can leave over (probe accuracy is a few ms)
MIN_HANDOFFS = 3
LOG = "/var/tmp/roles.log"
TOOLS = "/run/current-system/sw/bin"


def start_recorder(m):
    m.succeed(f"rm -f {LOG}")  # a transient unit appends to an old log instead of replacing it
    m.succeed(
        f"systemd-run --unit=roles --collect -p StandardOutput=file:{LOG} "
        f"{TOOLS}/stdbuf -oL {TOOLS}/drbdsetup events2 --timestamps all"
    )


def stop_recorder(m):
    m.succeed("systemctl stop roles.service")
    return m.succeed(f"cat {LOG}")


def offset_of(m):
    """m's clock minus the driver's, from the probe with the shortest round trip."""
    probes = []
    for _ in range(8):
        before = time.time()
        reading = float(m.succeed("date +%s.%N"))
        probes.append((before, reading, time.time()))
    return roles.clock_offset(probes)


def role_history(res, offsets, end):
    """Each node's Primary stretches in driver time, ending the recorders."""
    held = {}
    for m in NODES:
        history = roles.parse(stop_recorder(m), res)
        assert history, f"{m.name} recorded no role for {res}"
        held[m.name] = roles.primary_intervals([(when - offsets[m.name], role) for when, role in history], end=end)
    return held


def write_fails(m):
    """A direct 4 KiB write to the node's DRBD device is refused (no quorum) or never returns."""
    return m.execute(f"timeout 20 dd if=/dev/urandom of={device_of(m)} bs=4k count=1 oflag=direct conv=notrunc")[0] != 0


def show(held, since):
    for name, stretches in held.items():
        print(f"{name}: Primary for {[(round(a - since, 1), round(b - since, 1)) for a, b in stretches]}")


def dump_on_failure(res):
    for m in NODES:
        print(f"[{m.name}] drbd:\n{drbd_status(m, res)}")
        print(f"[{m.name}] volume:\n{m.execute('expanse ctl volume list 2>&1')[1]}")
        print(f"[{m.name}] agent:\n{m.execute('journalctl -u expansed.service -n 30 --no-pager 2>&1')[1]}")


def storm_round(rng, kind, res):
    """Cut nodes off (a pair leaves every node alone), hold, heal, hold."""
    primary = current_primary(res)
    spare = [m for m in NODES if m is not primary]
    cut = {"long": [primary], "short": [primary], "other": [rng.choice(spare)], "pair": rng.sample(NODES, 2)}[kind]
    length = rng.uniform(*CUT_S[kind])
    print(f"{kind}: cut {[m.name for m in cut]} (primary {primary.name}) for {length:.0f}s")
    for m in cut:
        m.block()
    time.sleep(length)
    for m in cut:
        m.unblock()
    time.sleep(rng.uniform(*HEAL_S))


def current_primary(res):
    """The one primary, or after a wait any node claiming the role, or the first node."""
    try:
        wait_for(lambda: single_primary(res) is not None, "one primary", 60)
    except Exception:
        pass
    return (primaries(res) or NODES)[0]


def single_primary(res):
    found = primaries(res)
    return found[0] if len(found) == 1 else None


def settled(res):
    """One primary, and it sees every replica UpToDate."""
    primary = single_primary(res)
    return primary is not None and fully_replicated(primary, res)


def state_of(m):
    return (volume_row(m, NAME) or {}).get("state")


form("vndp")

with subtest("a replication-3 volume is Healthy and has a primary"):
    n1.succeed(f"expanse ctl volume create {NAME} --size {SIZE_MIB}Mi --replication 3")
    wait_for(lambda: state_of(n1) == "healthy", "the volume to be Healthy", 120)
    res = volume_row(n1, NAME)["id"]
    wait_for(lambda: single_primary(res) is not None, "one primary")
    for m in NODES:
        start_recorder(m)
    offsets = {m.name: offset_of(m) for m in NODES}
    print(f"clock offsets from the driver: { {k: round(v, 4) for k, v in offsets.items()} }")

with subtest("a seeded partition storm that keeps cutting the primary off"):
    rng = random.Random(SEED)
    rounds = list(SCHEDULE)
    rng.shuffle(rounds)
    rounds.insert(0, "long")  # the first cut is always of the primary the volume started with
    began = time.time()
    try:
        for kind in rounds:
            storm_round(rng, kind, res)
    finally:
        for m in NODES:
            m.unblock()
    print(f"the storm took {time.time() - began:.0f}s")

with subtest("the volume converges: one primary, every replica UpToDate and identical"):
    try:
        wait_for(lambda: settled(res), "one primary and every replica UpToDate", 300)
        wait_for(lambda: state_of(n1) == "healthy", "the volume to be Healthy", 120)
    except Exception:
        dump_on_failure(res)
        raise
    primary = single_primary(res)
    for m in NODES:
        assert "StandAlone" not in drbd_status(m, res), f"{m.name} is split off:\n{drbd_status(m, res)}"
    primary.succeed(f"dd if=/dev/urandom of={device_of(primary)} bs=1M seek=3 count=4 oflag=direct conv=notrunc,fsync")
    time.sleep(3)
    sums = {m.name: checksum(m, f"/dev/vg0/{res}", SIZE_MIB) for m in NODES}
    assert len(set(sums.values())) == 1, f"replicas differ after the storm: {sums}"

with subtest("no two nodes were ever Primary at once"):
    finished = time.time()
    drift = {m.name: offset_of(m) - offsets[m.name] for m in NODES}
    print(f"clock drift over the run: { {k: round(v, 4) for k, v in drift.items()} }")
    held = role_history(res, offsets, finished)
    show(held, began)
    clashes = roles.overlaps(held, TOLERANCE_S)
    assert not clashes, f"two nodes were Primary together: {clashes}"
    moved = roles.handoffs(held)
    assert moved >= MIN_HANDOFFS, f"the storm moved the primary only {moved} times, which proves little"
    print(f"storm: {moved} handoffs, closest approach {roles.smallest_gap(held):.2f}s")

with subtest("control: a partitioned primary with a frozen agent is reported as a double primary"):
    for m in NODES:
        start_recorder(m)
    frozen = current_primary(res)
    others = [m for m in NODES if m is not frozen]
    began = time.time()
    frozen.succeed("systemctl kill --signal=SIGSTOP expansed.service")
    frozen.block()
    try:
        wait_for(lambda: len(primaries(res, others)) == 1, "the majority side to promote a node", 180)
        assert role_of(frozen, res) == "Primary", "the frozen node was expected to still hold the role"
        assert write_fails(frozen), "the partitioned old primary accepted a write"
        assert not write_fails(primaries(res, others)[0]), "the new primary refused a write"
    finally:
        frozen.unblock()
        frozen.succeed("systemctl kill --signal=SIGCONT expansed.service")
    wait_for(lambda: settled(res), "the volume to converge after the control", 300)
    held = role_history(res, offsets, time.time())
    show(held, began)
    clashes = roles.overlaps(held, TOLERANCE_S)
    assert clashes and frozen.name in (clashes[0].first, clashes[0].second), f"the checker missed a real double primary: {held}"
    print(f"control: detected {len(clashes)} overlap, {max(c.seconds for c in clashes):.1f}s long")
    print("VOL-NO-DOUBLE-PRIMARY PASSED")
