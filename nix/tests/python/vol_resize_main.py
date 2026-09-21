"""vol-resize (X3, part 1): a replicated DRBD volume grows online, under load, with no unmount.

A mounted ext4 volume takes a crc32c-verified fio load. `ctl volume resize` then grows it;
every replica's DRBD device and backing LV must follow, resize2fs must grow the filesystem
while it stays mounted, and fio must finish clean. Afterwards the filesystem is unmounted,
fsck must pass, and the grown range must be byte-identical on all three replicas.

Runs after cluster-common.py and vol_cluster.py.
"""

OLD_MIB = 256
NEW_MIB = 768
MNT = "/mnt/vol"
FIO_FILE = f"{MNT}/fio.dat"
FIO_UNIT = "fio-resize"
STOP_FILE = "/root/stop-fio"
FIO_TIMEOUT_S = 300
GROW_TIMEOUT_S = 90
VG = "vg0"


def size_of_device(m, res):
    """Bytes DRBD exposes for the resource, read from sysfs because a Secondary cannot be opened."""
    minor = m.succeed(f"drbdadm sh-minor {res}").strip()
    return int(m.succeed(f"cat /sys/block/drbd{minor}/size").strip()) * 512


def lv_bytes(m, res):
    return int(m.succeed(f"lvs --noheadings --units b --nosuffix -o lv_size {VG}/{res}").strip())


def digest_prefix(m, res, mib):
    return m.succeed(
        f"dd if=/dev/{VG}/{res} bs=1M count={mib} iflag=direct 2>/dev/null | sha256sum | cut -d' ' -f1"
    ).strip()


def fio_state(m):
    return m.execute(f"systemctl is-active {FIO_UNIT}.service")[1].strip()


form("volresize")

with subtest("volume created, replicated, formatted and mounted on the primary"):
    n1.succeed(f"expanse ctl volume create vresize --size {OLD_MIB}Mi --replication 3")
    for m in NODES:
        m.wait_until_succeeds("drbdadm status | grep -q '^vol-'", timeout=180)
    res = n1.succeed("drbdadm status | head -1 | cut -d' ' -f1").strip()
    wait_for(lambda: all(fully_replicated(m, res) for m in NODES), "every replica UpToDate", 300)
    wait_for(lambda: len(primaries(res)) == 1, "one primary")
    primary = primaries(res)[0]
    dev = device_of(primary)
    primary.succeed(f"mkfs.ext4 -F -q {dev}")
    primary.succeed(f"mkdir -p {MNT} && mount {dev} {MNT}")
    before = {m.name: size_of_device(m, res) for m in NODES}
    assert len(set(before.values())) == 1 and before[primary.name] >= OLD_MIB << 20, before
    print(f"primary {primary.name}, device {dev}, size before: {before[primary.name]}")

with subtest("grow online while a verified fio load runs"):
    primary.succeed(f"dd if=/dev/zero of={FIO_FILE} bs=1M count=32 conv=fsync")
    # Repeat verified passes until told to stop, so load spans the whole grow; a failed pass ends the unit nonzero.
    fio = f"/run/current-system/sw/bin/fio --name=resize --filename={FIO_FILE} --size=32M --bs=4k --rw=randwrite --direct=1 --verify=crc32c --do_verify=1 --verify_fatal=1"
    out = primary.succeed(
        f"rm -f {STOP_FILE}; systemd-run --unit={FIO_UNIT} bash -c 'while [ ! -e {STOP_FILE} ]; do {fio} || exit 1; done' 2>&1"
    )
    assert "Running as unit" in out, out
    time.sleep(3)  # let it start issuing I/O, so the grow lands mid-load
    n1.succeed(f"expanse ctl volume resize vresize --size {NEW_MIB}Mi")

with subtest("every replica's LV and DRBD device grows, and the volume resyncs"):
    wait_for(lambda: all(size_of_device(m, res) >= NEW_MIB << 20 for m in NODES), "DRBD devices to grow", GROW_TIMEOUT_S)
    for m in NODES:
        assert lv_bytes(m, res) >= NEW_MIB << 20, f"{m.name}'s LV did not grow"
    wait_for(lambda: all(fully_replicated(m, res) for m in NODES), "every replica UpToDate again", 300)
    assert len(primaries(res)) == 1, [m.name for m in primaries(res)]

with subtest("resize2fs grows the filesystem while it stays mounted"):
    print(primary.succeed(f"resize2fs {dev} 2>&1"))
    assert f"on {MNT} " in primary.succeed("mount"), "the volume was unmounted during the resize"
    if fio_state(primary) != "active":
        print(primary.execute(f"journalctl -u {FIO_UNIT}.service --no-pager | tail -n 40; dmesg | tail -n 20")[1])
        raise AssertionError("the fio load stopped before the grow finished")
    total = int(primary.succeed(f"df -B1 --output=size {MNT} | tail -1").strip())
    assert total > before[primary.name], f"filesystem is {total} bytes, no bigger than the old device"

with subtest("fio finished clean: no I/O errors, data intact"):
    primary.succeed(f"touch {STOP_FILE}")
    wait_for(lambda: fio_state(primary) != "active", "fio to finish", FIO_TIMEOUT_S)
    print(primary.execute(f"journalctl -u {FIO_UNIT}.service --no-pager | tail -n 25")[1])
    status = primary.succeed(f"systemctl show -p ExecMainStatus --value {FIO_UNIT}.service").strip()
    assert status == "0", f"fio exited {status}: an I/O error or a verify failure"
    assert primary.succeed("dmesg | grep -i 'I/O error' || true").strip() == "", "kernel I/O errors during the resize"

with subtest("the grown space takes writes, the filesystem is clean, all replicas agree"):
    primary.succeed(f"dd if=/dev/urandom of={MNT}/grown.dat bs=1M count=400 conv=fsync")
    primary.succeed(f"umount {MNT}")
    primary.succeed(f"fsck.ext4 -fn {dev}")
    wait_for(lambda: all(fully_replicated(m, res) for m in NODES), "every replica UpToDate", 120)
    sums = {m.name: digest_prefix(m, res, NEW_MIB) for m in NODES}
    assert len(set(sums.values())) == 1, f"replicas differ over the grown range: {sums}"
    print(f"VOL-RESIZE PASSED: {OLD_MIB} -> {NEW_MIB} MiB online under load, replicas identical")
