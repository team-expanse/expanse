"""vol-perf testScript body (G6.8, G6.9, G6.4, G6.7 budgets).

fio against an exvol R=3 volume and, on the same node and pool, a raw
local zvol created with the same properties; the ratios are checked
against test/perf/budgets.yaml (BUDGETS is injected by vol-perf.nix).
Then a secondary is stopped, drifted, and restarted to time the resync
(vol_resync_mbps), and the primary is hard-crashed under a write loop
to time failover (vol_failover_ms).

Spliced after cluster-common.py, vol_perf_lib.py, BUDGETS and
vol_perf_fio.py, which provide n1/n2/n3, form(), wait_agent_ready() and
the fio/ratio/budget helpers. Every phase runs before the final assert.
"""

VOL = "vperf"
RAW = "volumes/rawperf"
VOL_SIZE = "2Gi"
DRIFT_MB = 256


def vol_inspect(m):
    rc, out = m.execute(
        f"expanse ctl volume inspect {VOL} --socket /run/expanse/agent.sock 2>&1"
    )
    return out if rc == 0 else ""


def wait_primary_ready(timeout=120, exclude=None):
    deadline = time.time() + timeout
    while time.time() < deadline:
        for m in [n1, n2, n3]:
            if m is exclude:
                continue
            if m.execute("ls /dev/exvol 2>/dev/null")[1].strip():
                return m
        time.sleep(0.5)
    raise AssertionError(f"no ready primary within {timeout}s")


def replica_rows(out):
    return [
        ln.split()
        for ln in out.splitlines()
        if len(ln.split()) >= 5
        and ln.split()[1] in ("primary", "secondary", "stale", "resyncing")
    ]


def wait_all_current(m, timeout=180):
    deadline = time.time() + timeout
    while time.time() < deadline:
        rows = replica_rows(vol_inspect(m))
        if len(rows) == 3 and all(
            r[1] in ("primary", "secondary") and r[3] == "0" for r in rows
        ):
            return
        time.sleep(2)
    raise AssertionError(f"replicas never converged to lag 0 within {timeout}s")


form("volperf")

with subtest("volume created; raw local zvol with the same properties"):
    n1.succeed(f"expanse ctl volume create {VOL} --size {VOL_SIZE}")
    for m in [n1, n2, n3]:
        m.wait_until_succeeds(
            "zfs list -H -o name -t volume | grep -q '^volumes/volumes/vol-'", timeout=90
        )
    primary = wait_primary_ready()
    vol_id = primary.succeed("ls -1 /dev/exvol").strip()
    dev = "/dev/exvol/" + vol_id
    zvol_props = primary.succeed(
        f"zfs get -H -o property,value volblocksize,compression,sync,logbias,primarycache "
        f"volumes/volumes/{vol_id}"
    )
    create_args = " ".join(
        f"-o {p}={v}" for p, v in (ln.split("\t") for ln in zvol_props.strip().splitlines())
        if p != "volblocksize"
    )
    vbs = dict(ln.split("\t") for ln in zvol_props.strip().splitlines())["volblocksize"]
    primary.succeed(f"zfs create -V {VOL_SIZE.replace('i', '')} -b {vbs} {create_args} {RAW}")
    raw_dev = f"/dev/zvol/{RAW}"
    primary.wait_until_succeeds(f"test -b {raw_dev}", timeout=30)
    print(f"primary: {primary.name}  exvol: {dev}  raw: {raw_dev}  props: {zvol_props!r}")

problems = []

with subtest("fio profiles: exvol R=3 vs raw local zvol (G6.8, G6.9)"):
    fill(primary, dev)
    fill(primary, raw_dev)
    wait_all_current(primary)
    peer = "192.168.1.%d" % (2 if primary is n1 else 1)
    _, found = compare_devices(primary, raw_dev, dev, peer)
    problems += found

with subtest("resync rate after a secondary rejoins (G6.7)"):
    victim = [m for m in [n1, n2, n3] if m is not primary][-1]
    victim.succeed("systemctl stop expansed.service")
    primary.succeed(
        f"dd if=/dev/urandom of={dev} bs=4M count={DRIFT_MB // 4} conv=fsync,notrunc oflag=direct"
    )
    victim.succeed("systemctl start expansed.service")
    victim.wait_for_unit("expansed.service")
    wait_agent_ready(victim)
    start = time.time()
    wait_all_current(primary, timeout=120)
    elapsed = time.time() - start
    resync = mib_per_s(DRIFT_MB * 1024 * 1024, elapsed)
    print(f"resync: {DRIFT_MB} MiB drift caught up in {elapsed:.1f}s = {resync:.1f} MiB/s")
    problems += check_budgets({"vol_resync_mbps": resync}, BUDGETS)

with subtest("failover time: primary hard-crash to next successful write (G6.4)"):
    doomed = wait_primary_ready()
    doomed.succeed(f"dd if=/dev/urandom of=/dev/exvol/{vol_id} bs=4k count=1 oflag=direct conv=notrunc")
    start = time.time()
    doomed.crash()
    deadline = start + 120
    survivor = None
    while time.time() < deadline and survivor is None:
        for m in [n1, n2, n3]:
            if m is doomed:
                continue
            rc, _ = m.execute(
                f"test -b /dev/exvol/{vol_id} && "
                f"dd if=/dev/urandom of=/dev/exvol/{vol_id} bs=4k count=1 oflag=direct conv=notrunc 2>/dev/null"
            )
            if rc == 0:
                survivor = m
                break
        time.sleep(0.2)
    assert survivor is not None, "no node accepted a write within 120s of the primary crash"
    failover_ms = (time.time() - start) * 1000
    print(f"failover: {doomed.name} -> {survivor.name} writable after {failover_ms:.0f} ms")
    problems += check_budgets({"vol_failover_ms": failover_ms}, BUDGETS)

assert not problems, "perf budgets violated: " + "; ".join(problems)
print("VOL-PERF TEST PASSED: all storage budgets met")
