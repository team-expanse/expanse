"""vol-resize testScript body (G6.14).

A 10 GiB volume, formatted ext4 and mounted, under a crc32c-verified
fio write load; grow it to 20 GiB online (no unmount); resize2fs must
succeed; no I/O errors may occur during the operation; the fio load
must finish clean (its own embedded verify is the "data intact"
check for what was already on disk), and the newly grown space must
itself be usable.

Spliced (via readFile, see vol-resize.nix) after cluster-common.py,
which provides n1/n2/n3, form(), wait_agent_ready(), and friends.
"""

VOL = "vresize"
MNT = "/mnt/vol"
FIO_FILE = f"{MNT}/fio.dat"


def vol_inspect(m):
    rc, out = m.execute(
        f"expanse ctl volume inspect {VOL} --socket /run/expanse/agent.sock 2>&1"
    )
    return out if rc == 0 else ""


def wait_primary_ready(timeout=120):
    deadline = time.time() + timeout
    while time.time() < deadline:
        for m in [n1, n2, n3]:
            if m.execute("ls /dev/exvol 2>/dev/null")[1].strip():
                return m
        time.sleep(2)
    raise AssertionError(f"no ready primary within {timeout}s")


form("volresize")

with subtest("volume created, formatted ext4, mounted"):
    n1.succeed(f"expanse ctl volume create {VOL} --size 10Gi")
    for m in [n1, n2, n3]:
        m.wait_until_succeeds(
            "zfs list -H -o name -t volume | grep -q '^volumes/volumes/vol-'", timeout=90
        )
    primary = wait_primary_ready()
    vol_id = primary.succeed("ls -1 /dev/exvol").strip()
    dev = "/dev/exvol/" + vol_id
    primary.succeed(f"mkfs.ext4 -F {dev}")
    primary.succeed(f"mkdir -p {MNT}")
    primary.succeed(f"mount {dev} {MNT}")
    size_before = int(primary.succeed(f"blockdev --getsize64 {dev}").strip())
    print(f"primary: {primary.name}  vol_id: {vol_id}  size_before: {size_before}")

with subtest("start crc32c-verified fio load, resize online while it runs"):
    # Lay the file out first so the resize lands in fio's random-write
    # phase, not its (buffered, sequential) layout phase.
    primary.succeed(f"dd if=/dev/zero of={FIO_FILE} bs=1M count=64 conv=fsync")
    rc, out = primary.execute(
        "systemd-run --unit=fio-resize --description='resize fio load' "
        f"fio --name=resizetest --filename={FIO_FILE} --size=64M --bs=4k "
        "--rw=randwrite --direct=1 --verify=crc32c --do_verify=1 --verify_fatal=1 "
        "--numjobs=1 --group_reporting --eta=always --eta-newline=15 2>&1"
    )
    assert "Running as unit" in out, f"fio systemd-run failed to start: {out}"

    # Let fio actually start issuing I/O before resizing underneath it —
    # the point is to grow the volume WHILE the load is live (G6.14 "under
    # fio load"), not merely before/after it.
    time.sleep(3)

    n1.succeed(f"expanse ctl volume resize {VOL} --size 20Gi")

    # The zvol resize + in-place kernel NBD resize (netlink
    # NBD_CMD_RECONFIGURE, no reconnect) land asynchronously via the
    # runtime's reconcile tick, not synchronously with the CLI call.
    deadline = time.time() + 60
    size_after = size_before
    while time.time() < deadline:
        size_after = int(primary.succeed(f"blockdev --getsize64 {dev}").strip())
        if size_after > size_before:
            break
        time.sleep(2)
    assert size_after > size_before, (
        f"device size never grew past {size_before} within 60s (got {size_after})"
    )
    print(f"device grew: {size_before} -> {size_after}")

with subtest("resize2fs succeeds online — no unmount (G6.14)"):
    out = primary.succeed(f"resize2fs {dev} 2>&1")
    print(out)
    mount_out = primary.succeed("mount")
    assert f"on {MNT} " in mount_out, f"volume was unmounted during resize: {mount_out}"

with subtest("fio load finished clean: no I/O errors, data intact"):
    # Every 4k direct write is a synchronous 3-way quorum ack, then fio
    # reads it all back to verify — minutes, not seconds, inside a VM.
    deadline = time.time() + 420
    active = "active"
    while time.time() < deadline:
        rc, active = primary.execute("systemctl is-active fio-resize.service || true")
        if active.strip() != "active":
            break
        time.sleep(2)
    assert active.strip() != "active", "fio load never finished within 420s"

    print(primary.succeed("journalctl -u fio-resize.service --no-pager | tail -n 40"))
    status = primary.succeed(
        "systemctl show -p ExecMainStatus --value fio-resize.service"
    ).strip()
    assert status == "0", f"fio exited nonzero (ExecMainStatus={status}) — I/O error or verify failure"

    dmesg_errs = primary.succeed("dmesg | grep -i 'I/O error' || true").strip()
    assert dmesg_errs == "", f"kernel reported I/O errors during the resize: {dmesg_errs}"

with subtest("newly grown space is usable"):
    df_out = primary.succeed(f"df -B1 {MNT} | tail -1")
    print(df_out)
    total_bytes = int(df_out.split()[1])
    assert total_bytes > size_before, (
        f"filesystem size {total_bytes} did not grow past the original device size {size_before}"
    )

    primary.succeed(f"dd if=/dev/urandom of={MNT}/newspace.dat bs=1M count=64 conv=fsync")
    want = primary.succeed(f"sha256sum {MNT}/newspace.dat | cut -d' ' -f1").strip()
    primary.succeed("sync")
    got = primary.succeed(f"sha256sum {MNT}/newspace.dat | cut -d' ' -f1").strip()
    assert got == want, f"new-space data mismatch: {got} != {want}"

    print(
        "VOL-RESIZE TEST PASSED: online grow 10Gi->20Gi, resize2fs succeeded "
        "without unmount, no I/O errors, fio verify clean, new space usable"
    )
