"""VM-side fio jobs for vol-perf.

Spliced after vol_perf_lib.py and the BUDGETS definition; `m` is a NixOS test machine.
Nothing here asserts, so the caller can run every phase before it fails.
"""

RUNTIME_S = 15
RAMP_S = 3
RAND_IO_MIB = 256

# name -> (fio rw, block size, iodepth, result side)
PROFILES = {
    "seqwrite": ("write", "1M", 32, "write"),
    "seqread": ("read", "1M", 32, "read"),
    "randwrite": ("randwrite", "4k", 64, "write"),
    "randread": ("randread", "4k", 64, "read"),
}

# kind -> fio arguments for one pass over a device: sequential covers all of it, random scatters 4k writes over it
PASSES = {
    "seq": "--rw=write --bs=1M --iodepth=32 --size=100%",
    "rand": f"--rw=randwrite --bs=4k --iodepth=64 --io_size={RAND_IO_MIB}M",
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
    """Buffered 4k writes each followed by fsync: the sync clat is the flush latency the guest waits on."""
    out = m.succeed(
        f"fio --name=fsync --filename={dev} --ioengine=psync --rw=write --bs=4k "
        f"--fsync=1 --time_based --runtime={RUNTIME_S} --ramp_time={RAMP_S} "
        "--output-format=json"
    )
    return parse_fio(out, "write")


def one_pass(m, dev, kind):
    """One pass of the given kind; on a device never written it pays every first-touch cost."""
    out = m.succeed(f"fio --name=pass-{kind} --filename={dev} --direct=1 --ioengine=libaio {PASSES[kind]} --output-format=json")
    return parse_fio(out, "write")


def measure_all(m, dev):
    results = {p: fio(m, dev, p) for p in PROFILES}
    results["fsync"] = fio_fsync(m, dev)
    return results


def rate_line(name, r):
    return f"{name}: {r.iops:.0f} iops, {r.bw_bytes / 1048576:.1f} MiB/s"


def print_suite(label, results):
    for p in PROFILES:
        print(rate_line(f"{label} {p}", results[p]))
    print(f"{label} fsync p99: {results['fsync'].sync_p99_us} us")


def compare_suites(local, remote, rtt_us):
    """The replicated suite against the same node's local one: (budget values, problems the budgets do not cover)."""
    ratios = profile_ratios(local, remote)
    print("ratios:", {k: round(v, 3) for k, v in ratios.items()})
    fsync_us = remote["fsync"].sync_p99_us
    allowance = fsync_allowance_us(local["fsync"].sync_p99_us, rtt_us)
    print(f"fsync p99: local {local['fsync'].sync_p99_us} us, remote {fsync_us} us, allowance {allowance} us (2x local + RTT {rtt_us} us)")
    problems = [f"fsync p99 {fsync_us} us exceeds 2x local + 1 RTT = {allowance} us"] if fsync_us > allowance else []
    return dict(ratios, vol_fsync_p99_us=fsync_us), problems
