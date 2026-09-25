"""backup-volume-snapshot: PHASE-08-TASKS.md Stream B (X2). Snapshot a live
replicated volume, back the snapshot up with restic to a real in-VM garage S3
endpoint, and restore it -- content must be checksum-equal to the volume at
snapshot time, not just "the restore command exited 0". Also proves R1/D5's
concurrent-write concern for real: a writer keeps overwriting the LIVE volume
with pattern B while the SNAPSHOT device is read and backed up, and the
restored backup must still read back as pattern A, never a torn mix of A
and B.

Runs after cluster-common.py and vol_cluster.py.
"""

SIZE_MIB = 64
VG = "vg0"
SNAP = "snapbak"


def snapshot_dev(res):
    return f"/dev/{VG}/{res}-snap-{SNAP}"


def snapshot_listed(m):
    return SNAP in m.execute("expanse ctl volume inspect vbak 2>&1")[1]


form("volbak")

with subtest("volume created, replicated and holding pattern A"):
    n1.succeed(f"expanse ctl volume create vbak --size {SIZE_MIB}Mi --replication 3")
    for m in NODES:
        m.wait_until_succeeds("drbdadm status | grep -q '^vol-'", timeout=180)
    res = n1.succeed("drbdadm status | head -1 | cut -d' ' -f1").strip()
    wait_for(lambda: all(fully_replicated(m, res) for m in NODES), "every replica UpToDate", 300)
    wait_for(lambda: len(primaries(res)) == 1, "one primary")
    primary = primaries(res)[0]
    dev = device_of(primary)
    fill_paced(primary, dev, SIZE_MIB)
    ref_a = checksum(primary, dev, SIZE_MIB)

with subtest("snapshot: LV holds pattern A"):
    n1.succeed("expanse ctl volume snapshot vbak --name snapbak")
    wait_for(lambda: snapshot_listed(n1), "the snapshot to be recorded", 60)
    # A thin snapshot LV starts inactive (lvm.Snapshot's own doc comment);
    # volume.Restore activates it before reading, so this test must too.
    primary.succeed(f"lvchange --activate y --ignoreactivationskip {VG}/{res}-snap-{SNAP}")
    assert checksum(primary, snapshot_dev(res), SIZE_MIB) == ref_a, "snapshot LV does not hold A"

with subtest("garage and restic are up on n1"):
    n1.wait_for_unit("garage.service")
    n1.wait_for_open_port(3900)
    n1.wait_for_open_port(3901)

with subtest("bootstrap a single-node garage layout, S3 key and bucket"):
    node_id = n1.succeed(
        "garage status | awk 'NR>2 && NF {print $1; exit}'"
    ).strip()
    assert node_id, "no node id found in `garage status`"
    n1.succeed(f"garage layout assign -z dc1 -c 1G {node_id}")
    n1.succeed("garage layout apply --version 1")
    key_out = n1.succeed("garage key create restic-key")
    key_id = [l for l in key_out.splitlines() if l.startswith("Key ID:")][0].split(": ", 1)[1].strip()
    secret_key = [l for l in key_out.splitlines() if l.startswith("Secret key:")][0].split(": ", 1)[1].strip()
    n1.succeed("garage bucket create backups")
    n1.succeed("garage bucket allow --read --write --key restic-key backups")

env = (
    f"AWS_ACCESS_KEY_ID={key_id} "
    f"AWS_SECRET_ACCESS_KEY={secret_key} "
    "RESTIC_PASSWORD=expanse-vol-backup-password "
    "RESTIC_REPOSITORY=s3:http://127.0.0.1:3900/backups"
)

with subtest("restic init on the primary against the real garage endpoint"):
    primary.succeed(f"{env} restic init")

with subtest("concurrent write to the live volume while the snapshot is backed up"):
    # A background writer keeps overwriting the LIVE device with pattern B.
    # The snapshot device must stay isolated from it while restic reads it.
    # The marker lives under /tmp, not /root, so it cannot change /root's
    # own directory metadata and pollute the next subtest's dedupe check
    # (restic tracks a backed-up file's parent directory too).
    primary.succeed("mkdir -p /root/backup-src")
    primary.execute(
        f"(dd if=/dev/urandom of={dev} bs=1M count={SIZE_MIB} oflag=direct conv=fsync,notrunc; "
        "touch /tmp/writer-done) >/tmp/writer.log 2>&1 &"
    )
    primary.succeed(
        f"dd if={snapshot_dev(res)} of=/root/backup-src/snapshot.img bs=1M count={SIZE_MIB} iflag=direct conv=fsync"
    )
    out = primary.succeed(f"{env} restic backup /root/backup-src/snapshot.img")
    assert "Added to the repository: 0 B" not in out, out
    primary.wait_for_file("/tmp/writer-done", timeout=120)
    ref_b = checksum(primary, dev, SIZE_MIB)
    assert ref_b != ref_a, "concurrent writer did not change the live volume (bad test data)"

with subtest("the snapshot LV itself is unaffected by the concurrent live write"):
    assert checksum(primary, snapshot_dev(res), SIZE_MIB) == ref_a, "snapshot LV changed under a concurrent live write"

with subtest("second, unchanged backup of the snapshot dedupes to zero new bytes"):
    out = primary.succeed(f"{env} restic backup /root/backup-src/snapshot.img")
    assert "Added to the repository: 0 B" in out, f"dedupe did not kick in: {out}"

with subtest("restic check against the real backend"):
    primary.succeed(f"{env} restic check --read-data-subset=100%")

with subtest("restore is byte-for-byte pattern A, not the concurrent write"):
    primary.succeed("rm -rf /root/restore-out")
    primary.succeed(f"{env} restic restore latest --target /root/restore-out")
    restored = primary.succeed(
        "find /root/restore-out -name snapshot.img"
    ).strip().splitlines()[0]
    restored_sum = primary.succeed(f"sha256sum {restored}").split()[0]
    assert restored_sum == ref_a, "restored snapshot does not match pattern A"
    assert restored_sum != ref_b, "restored snapshot leaked the concurrent live write"

print("BACKUP-VOLUME-SNAPSHOT PASSED: LVM thin snapshot backed up and restored via restic against a real S3 endpoint, checksum-equal to pre-snapshot data and isolated from a concurrent live write")
