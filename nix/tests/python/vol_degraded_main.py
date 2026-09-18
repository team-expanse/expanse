"""vol-degraded testScript body (G6.11, G6.12).

R=3 volume. Hard-kill one secondary: the volume must report Degraded
and stay fully readable+writable. Hard-kill the second secondary too
(only the primary's own local copy left): the volume must report
ReadOnly, reads must keep succeeding from the primary's local copy,
and writes must fail fast with EIO — never hang (T09's lease/quorum-
loss EIO guarantee, proven here end to end against a real write-quorum
loss rather than a lease loss). Restore both and the volume must
recover to Healthy with all 3 replicas checksum-equal (G6.15).

The primary itself is never killed — that is vol-failover.nix/T17's
job; this test isolates the degrade/read-only/recover state machine
from primary re-election.

Spliced (via readFile, see vol-degraded.nix) after cluster-common.py,
which provides n1/n2/n3, form(), wait_agent_ready(), and friends.
"""

VOL = "vdeg"
BASE_MB = 256
BLK_MB = 4


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


def primary_node():
    for m in [n1, n2, n3]:
        out = vol_inspect(m)
        for ln in out.splitlines():
            if "primary:" in ln:
                pid = ln.split()[-1]
                mm = machine_by_name(pid)
                if mm is not None:
                    return mm
    raise AssertionError("no primary in inspect output")


def wait_primary_ready(timeout=120):
    deadline = time.time() + timeout
    while time.time() < deadline:
        for m in [n1, n2, n3]:
            if m.execute("ls /dev/exvol 2>/dev/null")[1].strip():
                return m
        time.sleep(2)
    raise AssertionError(f"no ready primary within {timeout}s")


def wait_state(primary, want, timeout=120):
    """Poll `expanse ctl volume inspect`'s state line — a live probe of
    the primary's own coordinator (liveState in cmd_volume_ops.go), not
    a store read, so it works even without raft quorum to persist a
    status update. Two different budgets matter depending on `want`:
    Healthy recovery is driven by the cluster's own node-liveness lease
    (~30s TTL + 15s skew), hence the generous default; but a ReadOnly
    check (both raft AND exvol write quorum lost at once, in this
    minimal 3-node topology) is racing the primary's OWN 10s volume-
    lease TTL — once that lapses unrenewed the primary safely refuses
    ALL further I/O, closing the very channel this probe dials — so a
    ReadOnly caller should pass a short timeout and get in and out
    quickly."""
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
        # iflag=direct: read the disk, not a page cache that may still
        # hold pre-resync content.
        f"dd iflag=direct if=/dev/zvol/volumes/volumes/{vol_id} bs=4M skip={skip} "
        f"count={blocks_4m} 2>/dev/null | sha256sum | cut -d' ' -f1"
    )
    assert rc == 0 and out.strip(), f"checksum read failed on {m.name}: {out}"
    return out.strip()


form("voldeg")

with subtest("volume created, primary attached, baseline written"):
    n1.succeed(f"expanse ctl volume create {VOL} --size 2Gi")
    for m in [n1, n2, n3]:
        m.wait_until_succeeds(
            "zfs list -H -o name -t volume | grep -q '^volumes/volumes/vol-'", timeout=90
        )
    primary = wait_primary_ready()
    vol_id = primary.succeed("ls -1 /dev/exvol").strip()
    dev = "/dev/exvol/" + vol_id
    print(f"primary: {primary.name}  vol_id: {vol_id}")
    primary.succeed(
        f"dd if=/dev/urandom of={dev} bs=4M count={BASE_MB // BLK_MB} conv=fsync,notrunc"
    )
    wait_all_current(primary, timeout=180)
    baseline = checksum_range(primary, vol_id, BASE_MB // BLK_MB)

secondaries = [m for m in [n1, n2, n3] if m.name != primary.name]
v1, v2 = secondaries[0], secondaries[1]
print(f"victims: {v1.name}, {v2.name} (primary stays {primary.name})")

with subtest("kill 1 secondary: Degraded, still readable and writable (G6.11)"):
    v1.crash()
    # A write here both proves the volume is still writable AND drives
    # the primary's own stale-replica detection (drainResults only
    # processes results from an in-flight op — an idle volume has none
    # to detect from).
    primary.succeed(
        f"dd if=/dev/urandom of={dev} bs=4M seek={BASE_MB // BLK_MB} "
        "count=1 conv=fsync,notrunc"
    )
    out = wait_state(primary, "Degraded")
    print(f"after 1 kill:\n{out}")
    got = checksum_range(primary, vol_id, BASE_MB // BLK_MB)
    assert got == baseline, f"baseline range changed while degraded: {got} != {baseline}"

with subtest("kill 2nd secondary: ReadOnly, reads ok, writes EIO not hang (G6.12)"):
    v2.crash()
    # Below write quorum (1 of 3 left: the primary's own local copy) —
    # the write must fail FAST, not hang. oflag=direct: a buffered
    # write (the default) returns instantly from the page cache and
    # only surfaces the failure at a later fsync — TWO separate
    # quorum-checked round trips (write, then flush) stacked back to
    # back. O_DIRECT forces the write itself to go straight through
    # the device synchronously, so this is a single round trip — the
    # only way to keep this comfortably inside the ~10s window the
    # primary's own cluster-membership lease stays valid without raft
    # quorum to renew it (see wait_state's docstring on that budget).
    start = time.time()
    rc, wout = primary.execute(
        f"timeout 20 dd if=/dev/urandom of={dev} bs=4M "
        f"seek={BASE_MB // BLK_MB + 1} count=1 oflag=direct 2>&1"
    )
    elapsed = time.time() - start
    assert rc != 0, f"write succeeded with only 1 of 3 replicas healthy: {wout}"
    assert rc != 124, f"write HUNG past the 20s `timeout` backstop (rc=124): {wout}"
    assert elapsed < 15, f"write took {elapsed:.1f}s to fail — looks like a hang, not fast EIO"
    print(f"write correctly failed in {elapsed:.1f}s: {wout.strip()!r}")

    out = wait_state(primary, "ReadOnly", timeout=15)
    print(f"after 2 kills:\n{out}")

    got = checksum_range(primary, vol_id, BASE_MB // BLK_MB)
    assert got == baseline, f"baseline range changed while read-only: {got} != {baseline}"

with subtest("restore both: Healthy, all 3 replicas checksum-equal (G6.15)"):
    for v in (v1, v2):
        v.start()
        v.wait_for_unit("expansed.service", timeout=180)
        wait_agent_ready(v)

    out = wait_state(primary, "Healthy", timeout=180)
    print(f"after restore:\n{out}")
    wait_all_current(primary, timeout=180)

    want = checksum_range(primary, vol_id, BASE_MB // BLK_MB)
    for m in [n1, n2, n3]:
        got = checksum_range(m, vol_id, BASE_MB // BLK_MB)
        assert got == want, f"{m.name} checksum {got} != primary's {want} (G6.15)"
    print(
        "VOL-DEGRADED TEST PASSED: Degraded/ReadOnly/Healthy transitions correct, "
        "reads always succeeded, writes EIO'd fast below quorum, checksums match"
    )
