"""vol-split-brain (B6): a split-brain moves the volume to NeedsManualRecovery and discards nothing.

With two replicas DRBD has no quorum, so the partitioned primary keeps taking writes until its lease
expires, and the peer is then promoted and takes writes of its own. When the link returns the two
histories have diverged: DRBD drops the connection on both sides and runs the split-brain handler.
The node records that, the volume lands in NeedsManualRecovery, and the controller stops touching it.
Both sides must still hold the data only they wrote, and stay disconnected until an operator decides.

A control first cuts the primary briefly without any write; that heals as an ordinary resync.

Runs after cluster-common.py and vol_cluster.py.
"""

SIZE_MIB = 128
NAME = "vsb"
MARKS = "/persist/expanse/split-brain"
OLD_WRITE_MIB = 5  # written on the old primary while it is cut off
NEW_WRITE_MIB = 9  # written on the promoted peer meanwhile


def state_of(m):
    return ((volume_row(m, NAME) or {}).get("state") or "").replace("_", "")


def in_sync(m, res):
    text = drbd_status(m, res)
    lines = text.split("\n")
    return len(lines) > 1 and "disk:UpToDate" in lines[1] and "peer-disk:UpToDate" in text


def region_sum(m, res, mib):
    """sha256 of one MiB of the backing device, which stays readable while the resource is Secondary."""
    return m.succeed(f"dd if=/dev/vg0/{res} bs=1M skip={mib} count=1 iflag=direct 2>/dev/null | sha256sum | cut -d' ' -f1").strip()


def write_region(m, res, mib):
    m.succeed(f"dd if=/dev/urandom of={device_of(m)} bs=1M seek={mib} count=1 oflag=direct conv=notrunc,fsync")
    return region_sum(m, res, mib)


def peer_dropped(m, res):
    return "connection:StandAlone" in drbd_status(m, res)


def dump_on_failure(res):
    for m in NODES:
        print(f"[{m.name}] drbd:\n{drbd_status(m, res)}")
        print(f"[{m.name}] volume:\n{m.execute('expanse ctl volume list 2>&1')[1]}")
        print(f"[{m.name}] kernel:\n{m.execute('journalctl -k --no-pager | grep -i drbd | tail -20')[1]}")
        print(f"[{m.name}] agent:\n{m.execute('journalctl -u expansed.service -n 20 --no-pager 2>&1')[1]}")


form("vsb")

with subtest("a replication-2 volume is Healthy with a primary and a peer"):
    n1.succeed(f"expanse ctl volume create {NAME} --size {SIZE_MIB}Mi --replication 2")
    wait_for(lambda: state_of(n1) == "healthy", "the volume to be Healthy", 120)
    row = volume_row(n1, NAME)
    res = row["id"]
    holders = [m for m in NODES if m.name in row["nodes"]]
    assert len(holders) == 2, f"expected two replicas, got {row}"
    wait_for(lambda: len(primaries(res, holders)) == 1, "one primary")
    wait_for(lambda: all(in_sync(m, res) for m in holders), "both replicas UpToDate")

with subtest("control: a brief cut with no write on either side is not a split-brain"):
    old = primaries(res, holders)[0]
    old.block()
    time.sleep(6)
    old.unblock()
    wait_for(lambda: all(in_sync(m, res) for m in holders), "the replicas to be UpToDate again", 120)
    time.sleep(6)
    assert state_of(n3) == "healthy", f"a clean reconnect changed the volume state to {state_of(n3)}"
    for m in holders:
        m.fail(f"test -e {MARKS}/{res}")

with subtest("the cut primary keeps writing while its peer is promoted and writes too"):
    old = primaries(res, holders)[0]
    new = [m for m in holders if m is not old][0]
    print(f"old primary {old.name}, peer {new.name}")
    old.block()
    try:
        old_sum = write_region(old, res, OLD_WRITE_MIB)
        wait_for(lambda: role_of(new, res) == "Primary", "the peer to be promoted", 180)
        new_sum = write_region(new, res, NEW_WRITE_MIB)
    finally:
        old.unblock()

with subtest("the volume lands in NeedsManualRecovery with both sides dropped"):
    try:
        wait_for(lambda: state_of(n3) == "needsmanualrecovery", "NeedsManualRecovery", 180)
        wait_for(lambda: all(peer_dropped(m, res) for m in holders), "both sides to drop the connection", 60)
    except Exception:
        dump_on_failure(res)
        raise
    for m in holders:
        m.succeed(f"test -e {MARKS}/{res}")
        assert "Split-Brain detected" in m.succeed("journalctl -k --no-pager"), f"{m.name}: the kernel did not report a split-brain"
    listing = n3.succeed("expanse ctl volume diverged")
    assert NAME in listing and "NeedsManualRecovery" in listing, f"volume diverged did not list the volume:\n{listing}"

with subtest("nothing is resolved: no primary, still dropped, and each side keeps what only it wrote"):
    wait_for(lambda: not primaries(res, holders), "the diverged volume to be taken out of service", 60)
    time.sleep(30)  # several agent ticks and a connect-int: nothing may reconnect or resync
    assert state_of(n3) == "needsmanualrecovery"
    for m in holders:
        assert peer_dropped(m, res), f"{m.name} reconnected:\n{drbd_status(m, res)}"
        assert "SyncSource" not in drbd_status(m, res) and "SyncTarget" not in drbd_status(m, res)
    zeros = n3.succeed("dd if=/dev/zero bs=1M count=1 2>/dev/null | sha256sum | cut -d' ' -f1").strip()
    assert region_sum(old, res, OLD_WRITE_MIB) == old_sum, "the old primary lost its own write"
    assert region_sum(new, res, NEW_WRITE_MIB) == new_sum, "the promoted peer lost its own write"
    assert region_sum(old, res, NEW_WRITE_MIB) == zeros, "the old primary received the peer's write"
    assert region_sum(new, res, OLD_WRITE_MIB) == zeros, "the promoted peer received the old primary's write"
    print("VOL-SPLIT-BRAIN PASSED")
