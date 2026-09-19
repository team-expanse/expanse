"""vol-snapshot testScript body (G6.13).

Write A, take a named snapshot, write B over the same range, restore
the snapshot: content must be back to A, and every replica (not just
the primary's own zvol) must agree once the restore-triggered resync
of the other two converges.

No node ever crashes in this test — restore is exercised as a live
primary rolling its own zvol back and marking its secondaries Stale;
vol-degraded.nix/vol-resync-incremental.nix already cover the crash/
resync machinery this reuses.

Spliced (via readFile, see vol-snapshot.nix) after cluster-common.py,
which provides n1/n2/n3, form(), wait_agent_ready(), and friends.
"""

VOL = "vsnap"
BASE_MB = 256
BLK_MB = 4
SNAP_NAME = "snapA"


def vol_inspect(m):
    rc, out = m.execute(
        f"expanse ctl volume inspect {VOL} --socket /run/expanse/agent.sock 2>&1"
    )
    return out if rc == 0 else ""


def machine_by_name(name):
    for mm in [n1, n2, n3]:
        if mm.name == name:
            return mm
    return None


def state_of(out):
    for ln in out.splitlines():
        if ln.strip().startswith("state:"):
            return ln.split()[-1]
    return ""


def wait_primary_ready(timeout=120):
    deadline = time.time() + timeout
    while time.time() < deadline:
        for m in [n1, n2, n3]:
            if m.execute("ls /dev/exvol 2>/dev/null")[1].strip():
                return m
        time.sleep(2)
    raise AssertionError(f"no ready primary within {timeout}s")


def wait_state(primary, want, timeout=60):
    deadline = time.time() + timeout
    out = ""
    while time.time() < deadline:
        out = vol_inspect(primary)
        if state_of(out) == want:
            return out
        time.sleep(1)
    raise AssertionError(f"state never reached {want} within {timeout}s:\n{out}")


def replica_rows(out):
    return [
        ln.split()
        for ln in out.splitlines()
        if len(ln.split()) >= 5
        and ln.split()[1] in ("primary", "secondary", "stale", "resyncing")
    ]


def wait_all_current(primary, timeout=180):
    deadline = time.time() + timeout
    out = ""
    while time.time() < deadline:
        out = vol_inspect(primary)
        rows = replica_rows(out)
        if len(rows) == 3 and all(
            r[1] in ("primary", "secondary") and r[3] == "0" for r in rows
        ):
            return out
        time.sleep(3)
    raise AssertionError(f"replicas never converged to lag 0 within {timeout}s:\n{out}")


def checksum_range(m, vol_id, blocks_4m, skip=0):
    rc, out = m.execute(
        f"dd iflag=direct if=/dev/zvol/volumes/volumes/{vol_id} bs=4M skip={skip} "
        f"count={blocks_4m} 2>/dev/null | sha256sum | cut -d' ' -f1"
    )
    assert rc == 0 and out.strip(), f"checksum read failed on {m.name}: {out}"
    return out.strip()


form("volsnap")

with subtest("volume created, primary attached"):
    n1.succeed(f"expanse ctl volume create {VOL} --size 2Gi")
    for m in [n1, n2, n3]:
        m.wait_until_succeeds(
            "zfs list -H -o name -t volume | grep -q '^volumes/volumes/vol-'", timeout=90
        )
    primary = wait_primary_ready()
    vol_id = primary.succeed("ls -1 /dev/exvol").strip()
    dev = "/dev/exvol/" + vol_id
    print(f"primary: {primary.name}  vol_id: {vol_id}")

with subtest("write A, snapshot, write B over the same range"):
    primary.succeed(
        f"dd if=/dev/urandom of={dev} bs=4M count={BASE_MB // BLK_MB} conv=fsync,notrunc"
    )
    wait_all_current(primary, timeout=180)
    checksum_a = checksum_range(primary, vol_id, BASE_MB // BLK_MB)

    primary.succeed(f"expanse ctl volume snapshot {VOL} --name {SNAP_NAME}")
    # The CLI only queues the op; the agent snapshots on its next tick.
    # Writing B before the snapshot exists would make it capture A+B.
    primary.wait_until_succeeds(
        f"zfs list -H -t snapshot -o name | grep -q '/{vol_id}@{SNAP_NAME}$'", timeout=60
    )

    primary.succeed(
        f"dd if=/dev/urandom of={dev} bs=4M count={BASE_MB // BLK_MB} conv=fsync,notrunc"
    )
    wait_all_current(primary, timeout=180)
    checksum_b = checksum_range(primary, vol_id, BASE_MB // BLK_MB)
    assert checksum_b != checksum_a, "write B produced the same content as A (bad test data)"

with subtest("restore snapshot: content == A, B is gone (G6.13)"):
    primary.succeed(f"expanse ctl volume restore {VOL} --snapshot {SNAP_NAME}")

    deadline = time.time() + 60
    got = ""
    while time.time() < deadline:
        got = checksum_range(primary, vol_id, BASE_MB // BLK_MB)
        if got == checksum_a:
            break
        time.sleep(2)
    assert got == checksum_a, f"primary content after restore = {got}, want A = {checksum_a}"
    assert got != checksum_b, "primary still shows B's content after restore"

with subtest("all replicas agree post-restore (G6.13, G6.15)"):
    wait_all_current(primary, timeout=180)
    for m in [n1, n2, n3]:
        got = checksum_range(m, vol_id, BASE_MB // BLK_MB)
        assert got == checksum_a, f"{m.name} checksum {got} != restored A {checksum_a}"
    print("VOL-SNAPSHOT TEST PASSED: restore reverted to A, B is gone, all replicas agree")
