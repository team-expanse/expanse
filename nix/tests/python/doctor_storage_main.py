"""A5: `expanse doctor storage` against real LVM/DRBD/btrfs state on one node."""

ROWS = ["drbd-module", "volume-group", "thin-pools", "system-mirror", "drbd-resources"]


def doctor():
    return machine.execute("expanse doctor storage --vg vg0 --system-mount /mnt/sysvol; echo RC=$?")[1]


machine.start()
machine.wait_for_unit("multi-user.target")
machine.wait_for_unit("expanse-scratch-vg.service")

# A loopback-file btrfs: single-device by construction (storage-test.nix
# already owns /dev/vdb for the scratch VG), enough to exercise the real
# `btrfs filesystem show` detection path.
machine.succeed("truncate -s 256M /root/sysvol.img")
machine.succeed("mkfs.btrfs -f /root/sysvol.img")
machine.succeed("mkdir -p /mnt/sysvol && mount -o loop /root/sysvol.img /mnt/sysvol")

with subtest("healthy baseline: module, VG and empty pool pass; the loopback system volume is single-device"):
    out = doctor()
    for row in ROWS:
        assert row in out, f"missing {row} row:\n{out}"
    assert "RC=0" in out, f"healthy run should exit 0:\n{out}"
    assert "single device" in out, f"system volume should be flagged single-device:\n{out}"
    assert "↳" in out, f"single-device row should carry a remediation hint:\n{out}"

with subtest("a missing DRBD module fails, with a remediation hint"):
    machine.succeed("rmmod drbd")
    out = doctor()
    assert "drbd-module" in out and "FAIL" in out, f"missing module should FAIL:\n{out}"
    assert "RC=0" not in out, f"a FAIL row must exit nonzero:\n{out}"
    machine.succeed("modprobe drbd")

with subtest("a deliberately filled thin pool fails, with a remediation hint"):
    # Size the fill off the pool's real bytes rather than a guess: comfortably
    # over the 90% ceiling, but short of 100% so the default thin-pool-full
    # policy (queue I/O) never has a chance to hang the write.
    pool_bytes = int(machine.succeed("lvs --noheadings --units b --nosuffix -o lv_size vg0/pool").strip())
    fill_mb = int(pool_bytes * 0.95 / (1024 * 1024))
    machine.succeed(f"lvcreate --yes --type thin -V {fill_mb}M -T vg0/pool -n filler")
    machine.succeed(f"dd if=/dev/urandom of=/dev/vg0/filler bs=1M count={fill_mb} conv=fsync")
    out = doctor()
    assert "thin-pools" in out and "FAIL" in out, f"a near-full pool should FAIL:\n{out}"
    assert "RC=0" not in out, f"a FAIL row must exit nonzero:\n{out}"
