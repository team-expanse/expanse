"""vol-drbd-spike testScript body.

DRBD 9 (protocol C, quorum majority) over three zvols with the same
properties exvol uses. Measures the fio ratios against a raw zvol, the
initial-sync and incremental-resync rates, and the time from a primary
hard-crash to the next successful write on a promoted survivor, plus a
read-back check of an acked block. Every phase runs before the final
assert so one miss does not hide the other numbers.

Spliced after vol_perf_lib.py, BUDGETS and vol_perf_fio.py.
"""

import time

MACHINES = [n1, n2, n3]
ZVOL_ARGS = "-b 16k -o compression=zstd -o sync=always -o logbias=throughput -o primarycache=metadata"
DRBD = "/dev/drbd0"
RAW_DEV = "/dev/zvol/volumes/rawperf"
DRIFT_MB = 256
PEER_UP = "peer-disk:UpToDate"


def status(m):
    return m.succeed("drbdadm status r0")


def wait_all_uptodate(m, timeout):
    """Block until the primary reports itself and both peers UpToDate."""
    deadline = time.time() + timeout
    out = ""
    while time.time() < deadline:
        out = status(m)
        if "disk:UpToDate" in out and out.count(PEER_UP) == 2:
            return
        time.sleep(0.2)
    raise AssertionError(f"not all replicas UpToDate within {timeout}s:\n{out}")


start_all()
for m in MACHINES:
    m.wait_for_unit("multi-user.target")
    m.wait_for_unit("expanse-scratch-pool.service")

problems = []

with subtest("DRBD 9 module loaded (not the in-tree 8.4)"):
    for m in MACHINES:
        m.succeed("modprobe drbd")
        version = m.succeed("cat /sys/module/drbd/version").strip()
        print(f"{m.name}: drbd module {version}")
        assert version.startswith("9."), f"{m.name} loaded drbd {version}, need 9.x"

with subtest("zvols, metadata, connect, initial full sync"):
    for m in MACHINES:
        m.succeed(f"zfs create -V 2G {ZVOL_ARGS} volumes/drbdperf")
        m.wait_until_succeeds("test -b /dev/zvol/volumes/drbdperf", timeout=30)
        m.succeed("drbdadm create-md --force r0")
        m.succeed("drbdadm up r0")
    n1.succeed(f"zfs create -V 2G {ZVOL_ARGS} volumes/rawperf")
    n1.wait_until_succeeds(f"test -b {RAW_DEV}", timeout=30)
    start = time.time()
    n1.succeed("drbdadm primary --force r0")
    wait_all_uptodate(n1, timeout=300)
    initial = mib_per_s(2 * 1024 * 1024 * 1024, time.time() - start)
    print(f"initial full sync of 2 GiB: {initial:.1f} MiB/s")
    print(status(n1))

with subtest("fio profiles: DRBD R=3 vs raw local zvol"):
    fill(n1, DRBD)
    fill(n1, RAW_DEV)
    wait_all_uptodate(n1, timeout=120)
    _, found = compare_devices(n1, RAW_DEV, DRBD, "192.168.1.2")
    problems += found

with subtest("incremental resync rate after a secondary rejoins"):
    n3.succeed("drbdadm down r0")
    n1.succeed(f"dd if=/dev/urandom of={DRBD} bs=4M count={DRIFT_MB // 4} oflag=direct conv=notrunc")
    n3.succeed("drbdadm up r0")
    start = time.time()
    wait_all_uptodate(n1, timeout=120)
    elapsed = time.time() - start
    resync = mib_per_s(DRIFT_MB * 1024 * 1024, elapsed)
    print(f"resync: {DRIFT_MB} MiB drift in {elapsed:.1f}s = {resync:.1f} MiB/s")
    problems += check_budgets({"vol_resync_mbps": resync}, BUDGETS)

with subtest("primary hard-crash: promote a survivor, acked block intact"):
    n1.succeed("dd if=/dev/urandom of=/tmp/pat bs=1M count=1")
    n1.succeed(f"dd if=/tmp/pat of={DRBD} bs=1M seek=1 oflag=direct conv=notrunc")
    want = n1.succeed("sha256sum /tmp/pat | cut -d' ' -f1").strip()
    start = time.time()
    n1.crash()
    survivor = n2
    deadline = start + 120
    while time.time() < deadline:
        rc, _ = survivor.execute("drbdadm primary r0 2>&1")
        if rc == 0:
            rc, _ = survivor.execute(f"dd if=/dev/urandom of={DRBD} bs=4k count=1 oflag=direct conv=notrunc 2>/dev/null")
            if rc == 0:
                break
        time.sleep(0.2)
    else:
        raise AssertionError("survivor never became a writable primary within 120s")
    failover_ms = (time.time() - start) * 1000
    print(f"failover: promote + first write {failover_ms:.0f} ms after the crash")
    got = survivor.succeed(
        f"dd iflag=direct if={DRBD} bs=1M count=1 skip=1 2>/dev/null | sha256sum | cut -d' ' -f1"
    ).strip()
    assert got == want, f"acked block changed across failover: {got} != {want}"
    problems += check_budgets({"vol_failover_ms": failover_ms}, BUDGETS)

assert not problems, "perf budgets violated: " + "; ".join(problems)
print("VOL-DRBD-SPIKE PASSED")
