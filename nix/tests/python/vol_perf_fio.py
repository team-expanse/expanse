"""VM-side fio comparison shared by vol-perf and vol-drbd-spike.

Spliced after vol_perf_lib.py and the BUDGETS definition; `m` is a
NixOS test machine. Returns findings instead of asserting so callers
can run every phase before failing.
"""

RUNTIME_S = 15
RAMP_S = 3

# name -> (fio rw, block size, iodepth, result side)
PROFILES = {
    "seqwrite": ("write", "1M", 32, "write"),
    "seqread": ("read", "1M", 32, "read"),
    "randwrite": ("randwrite", "4k", 64, "write"),
    "randread": ("randread", "4k", 64, "read"),
}


def fio(m, dev, profile):
    rw, bs, depth, side = PROFILES[profile]
    out = m.succeed(
        f"fio --name={profile} --filename={dev} --direct=1 --ioengine=libaio "
        f"--rw={rw} --bs={bs} --iodepth={depth} --time_based "
        f"--runtime={RUNTIME_S} --ramp_time={RAMP_S} --output-format=json"
    )
    return parse_fio(out, side)


def fio_fsync(m, dev):
    """Buffered 4k writes each followed by fsync: the sync clat is the
    flush latency the guest application actually waits on."""
    out = m.succeed(
        f"fio --name=fsync --filename={dev} --ioengine=psync --rw=write --bs=4k "
        f"--fsync=1 --time_based --runtime={RUNTIME_S} --ramp_time={RAMP_S} "
        "--output-format=json"
    )
    return parse_fio(out, "write")


def fill(m, dev):
    """Write the whole device once so reads hit written blocks, not holes."""
    m.succeed(f"fio --name=fill --filename={dev} --rw=write --bs=1M --direct=1 --size=100% >/dev/null")


def measure_all(m, dev):
    results = {p: fio(m, dev, p) for p in PROFILES}
    results["fsync"] = fio_fsync(m, dev)
    return results


def rate_line(name, r):
    return f"{name}: {r.iops:.0f} iops, {r.bw_bytes / 1048576:.1f} MiB/s"


def compare_devices(m, raw_dev, dev, peer_ip):
    """Run every profile on the raw and replicated devices; return
    (measured budget values, list of budget problems)."""
    local = measure_all(m, raw_dev)
    remote = measure_all(m, dev)
    for p in PROFILES:
        print(rate_line(f"local  {p}", local[p]))
        print(rate_line(f"remote {p}", remote[p]))
    ratios = profile_ratios(local, remote)
    print("ratios:", {k: round(v, 3) for k, v in ratios.items()})

    rtt_us = ping_max_rtt_us(m.succeed(f"ping -c 20 -i 0.2 -q {peer_ip}"))
    fsync_us = remote["fsync"].sync_p99_us
    allowance = fsync_allowance_us(local["fsync"].sync_p99_us, rtt_us)
    print(
        f"fsync p99: local {local['fsync'].sync_p99_us} us, remote {fsync_us} us, "
        f"allowance {allowance} us (2x local + RTT {rtt_us} us)"
    )

    measured = dict(ratios, vol_fsync_p99_us=fsync_us)
    problems = check_budgets(measured, BUDGETS)
    if fsync_us > allowance:
        problems.append(f"fsync p99 {fsync_us} us exceeds 2x local + 1 RTT = {allowance} us")
    return measured, problems
