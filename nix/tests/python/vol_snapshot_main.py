"""vol-snapshot: snapshot a live replicated volume, overwrite it, restore it.

Write A, snapshot, write B over the same range, restore: the DRBD device and every replica's
LV must hold A again. The restore is a copy through the DRBD device, so it must refuse to
run under a mounted filesystem (the kernel's exclusive open) and proceed once it is unmounted.
Deleting the volume must take its snapshot LVs with it.

Runs after cluster-common.py and vol_cluster.py.
"""

SIZE_MIB = 256
VG = "vg0"
SNAP = "snapa"
MNT = "/mnt/vol"


def snapshot_lv(name):
    return f"/dev/{VG}/{name}-snap-{SNAP}"


def snapshot_listed(m):
    return SNAP in m.execute("expanse ctl volume inspect vsnap 2>&1")[1]


def replicas_hold(ref):
    return all(checksum(m, f"/dev/{VG}/{res}", SIZE_MIB) == ref for m in NODES)


def lvs_left(m):
    return m.succeed("lvs --noheadings -o lv_name vg0 | grep -c '^ *vol-' || true").strip()


form("volsnap")

with subtest("volume created, replicated and holding A"):
    n1.succeed(f"expanse ctl volume create vsnap --size {SIZE_MIB}Mi --replication 3")
    for m in NODES:
        m.wait_until_succeeds("drbdadm status | grep -q '^vol-'", timeout=180)
    res = n1.succeed("drbdadm status | head -1 | cut -d' ' -f1").strip()
    wait_for(lambda: all(fully_replicated(m, res) for m in NODES), "every replica UpToDate", 300)
    wait_for(lambda: len(primaries(res)) == 1, "one primary")
    primary = primaries(res)[0]
    dev = device_of(primary)
    fill_paced(primary, dev, SIZE_MIB)
    ref_a = checksum(primary, dev, SIZE_MIB)

with subtest("snapshot: recorded, held by the primary, an LV on it"):
    n1.succeed("expanse ctl volume snapshot vsnap --name snapa")
    wait_for(lambda: snapshot_listed(n1), "the snapshot to be recorded", 60)
    inspect = n1.succeed("expanse ctl volume inspect vsnap")
    assert re.search(rf"{SNAP}\s+{primary.name}\b", inspect), f"snapshot not held by the primary {primary.name}:\n{inspect}"
    origin = primary.succeed(f"lvs --noheadings -o origin {VG}/{res}-snap-{SNAP}").strip()
    assert origin == res, f"snapshot LV origin is {origin!r}, want {res}"

with subtest("write B over the same range"):
    fill_paced(primary, dev, SIZE_MIB)
    wait_for(lambda: all(fully_replicated(m, res) for m in NODES), "every replica UpToDate", 300)
    ref_b = checksum(primary, dev, SIZE_MIB)
    assert ref_b != ref_a, "write B produced the same content as A (bad test data)"

with subtest("restore: every replica holds A again and B is gone"):
    n1.succeed("expanse ctl volume restore vsnap --snapshot snapa")
    wait_for(lambda: checksum(primary, dev, SIZE_MIB) == ref_a, "the primary to hold A", 180)
    wait_for(lambda: all(fully_replicated(m, res) for m in NODES), "every replica UpToDate", 300)
    wait_for(lambda: replicas_hold(ref_a), "every replica's LV to hold A", 60)
    assert checksum(primary, snapshot_lv(res), SIZE_MIB) == ref_a, "the snapshot did not keep A through B and the restore"

with subtest("restore refuses a mounted volume, then runs once it is unmounted"):
    primary.succeed(f"mkfs.ext4 -q -F {dev} && mkdir -p {MNT} && mount {dev} {MNT} && echo canary > {MNT}/canary && sync")
    n1.succeed("expanse ctl volume restore vsnap --snapshot snapa")
    time.sleep(10)  # several sync passes; the copy must keep failing on the exclusive open
    primary.succeed(f"grep -q canary {MNT}/canary")
    journal = primary.succeed("journalctl -u expansed.service --no-pager")
    assert "exclusively" in journal, "no refusal logged: the restore did not even try, or ran under the mount"
    primary.succeed(f"umount {MNT}")
    wait_for(lambda: checksum(primary, dev, SIZE_MIB) == ref_a, "the queued restore to run after the unmount", 180)
    wait_for(lambda: all(fully_replicated(m, res) for m in NODES), "every replica UpToDate", 300)
    wait_for(lambda: replicas_hold(ref_a), "every replica's LV to hold A", 60)

with subtest("deleting the volume deletes its snapshots"):
    n1.succeed("expanse ctl volume delete vsnap")
    for m in NODES:
        wait_for(lambda m=m: lvs_left(m) == "0", f"{m.name} to drop every vol LV", 180)
    print("VOL-SNAPSHOT PASSED: restore reverted to A on every replica, a mounted volume was refused, delete removed the snapshots")
