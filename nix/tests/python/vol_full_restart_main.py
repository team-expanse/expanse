"""vol-full-restart testScript body (G6.10).

Write 2 GiB to an R=3 volume, checksum it, stop the daemon on ALL 3
nodes at once (a full cluster restart, not a single-node failover —
`cluster-full-restart.nix`'s exvol analog), start all 3 back up, and
assert the volume becomes Healthy again within 60 s with its checksum
intact. Nothing here is a crash/data-loss scenario (every write was
quorum-committed and durably logged on all 3 before the stop), so this
exercises re-election + reconnect + device re-attach, not resync.

Spliced (via readFile, see vol-full-restart.nix) after
cluster-common.py, which provides n1/n2/n3, form(), wait_agent_ready(),
and friends.
"""

VOL = "vfr"
TWO_GIB_MB = 2 * 1024
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


def wait_healthy(timeout=60):
    """Poll for state Healthy, re-resolving the primary each round —
    a full restart may re-elect a different node than before."""
    deadline = time.time() + timeout
    out = ""
    while time.time() < deadline:
        try:
            out = vol_inspect(primary_node())
            if state_of(out) == "Healthy":
                return out
        except AssertionError:
            pass
        time.sleep(2)
    raise AssertionError(f"volume not Healthy within {timeout}s:\n{out}")


def replica_rows(out):
    return [
        ln.split()
        for ln in out.splitlines()
        if len(ln.split()) >= 5
        and ln.split()[1] in ("primary", "secondary", "stale", "resyncing")
    ]


def wait_all_current(timeout=120):
    deadline = time.time() + timeout
    out = ""
    while time.time() < deadline:
        out = vol_inspect(primary_node())
        rows = replica_rows(out)
        if len(rows) == 3 and all(
            r[1] in ("primary", "secondary") and r[3] == "0" for r in rows
        ):
            return out
        time.sleep(3)
    raise AssertionError(f"replicas never converged to lag 0 within {timeout}s:\n{out}")


def paced_write(m, dev, seek_4m, total_blocks_4m, chunk_blocks=64, pause=0.5):
    """Write in bursts with a pause between them — a single
    uninterrupted multi-GiB dd saturates this VM harness's shared
    virtual network link (WireGuard-encrypted exp0 replication traffic
    and raft's control-plane traffic share it) for long enough to trip
    a spurious raft leader election mid-write. See the identical helper
    in vol_resync_incremental_main.py for the full rationale."""
    off = seek_4m
    remaining = total_blocks_4m
    while remaining > 0:
        n = min(chunk_blocks, remaining)
        m.succeed(f"dd if=/dev/urandom of={dev} bs=4M seek={off} count={n} conv=fsync,notrunc")
        off += n
        remaining -= n
        if remaining > 0:
            time.sleep(pause)


def checksum_all(m, vol_id, blocks_4m):
    rc, out = m.execute(
        # iflag=direct: read the disk, not a page cache that may still
        # hold pre-restart content.
        f"dd iflag=direct if=/dev/zvol/volumes/volumes/{vol_id} bs=4M "
        f"count={blocks_4m} 2>/dev/null | sha256sum | cut -d' ' -f1"
    )
    assert rc == 0 and out.strip(), f"checksum read failed on {m.name}: {out}"
    return out.strip()


form("volfull")

with subtest("volume created, primary attached, 2 GiB written and converged"):
    n1.succeed(f"expanse ctl volume create {VOL} --size 2Gi")
    for m in [n1, n2, n3]:
        m.wait_until_succeeds(
            "zfs list -H -o name -t volume | grep -q '^volumes/volumes/vol-'", timeout=90
        )
    primary = wait_primary_ready()
    vol_id = primary.succeed("ls -1 /dev/exvol").strip()
    dev = "/dev/exvol/" + vol_id
    print(f"primary: {primary.name}  vol_id: {vol_id}")
    paced_write(primary, dev, 0, TWO_GIB_MB // BLK_MB)
    wait_all_current(timeout=180)

with subtest("pre-stop reference: all 3 replicas already checksum-equal"):
    blocks = TWO_GIB_MB // BLK_MB
    ref = checksum_all(primary, vol_id, blocks)
    for m in [n1, n2, n3]:
        got = checksum_all(m, vol_id, blocks)
        assert got == ref, f"{m.name} checksum {got} != primary's {ref} before stop"
    print(f"pre-stop reference checksum: {ref}")

with subtest("stop ALL 3 node daemons"):
    for m in [n1, n2, n3]:
        m.succeed("systemctl stop expansed.service")

with subtest("start all 3; volume Healthy within 60s (G6.10); checksum matches (G6.15)"):
    start = time.time()
    for m in [n1, n2, n3]:
        m.succeed("systemctl start expansed.service")
        m.wait_for_unit("expansed.service", timeout=60)
    for m in [n1, n2, n3]:
        wait_agent_ready(m)

    wait_healthy(timeout=60)
    elapsed = time.time() - start
    assert elapsed < 60, f"volume took {elapsed:.1f}s to reach Healthy (> 60s budget, G6.10)"
    print(f"volume Healthy again in {elapsed:.1f}s")

    wait_all_current(timeout=60)
    blocks = TWO_GIB_MB // BLK_MB
    for m in [n1, n2, n3]:
        got = checksum_all(m, vol_id, blocks)
        assert got == ref, f"{m.name} checksum {got} != pre-stop reference {ref} (G6.15)"
    print(
        f"VOL-FULL-RESTART TEST PASSED: Healthy in {elapsed:.1f}s (< 60s), "
        "all 3 replicas checksum-equal to the pre-stop reference"
    )
