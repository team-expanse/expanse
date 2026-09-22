"""Pure helpers for the vol-perf VM test: fio parsing, ratios, budget checks.

Spliced ahead of vol_perf_main.py in the testScript, and imported by
vol_perf_lib_test.py, so it depends only on the standard library.
"""

import json
from dataclasses import dataclass


@dataclass
class FioResult:
    iops: float
    bw_bytes: float
    sync_p99_us: "float | None"


def parse_fio(text, direction):
    """Parse `fio --output-format=json` output for one job's read or write side."""
    start = text.find("{")
    if start < 0:
        raise ValueError(f"no JSON in fio output: {text[:200]!r}")
    job = json.loads(text[start:])["jobs"][0]
    side = job[direction]
    p99_ns = job.get("sync", {}).get("lat_ns", {}).get("percentile", {}).get("99.000000")
    return FioResult(
        iops=side["iops"],
        bw_bytes=side["bw_bytes"],
        sync_p99_us=None if p99_ns is None else p99_ns / 1000,
    )


def ratio(vol, local):
    if local <= 0:
        raise ValueError("local baseline is zero; cannot form a ratio")
    return vol / local


def profile_ratios(local, remote):
    """Budget-named ratios: bandwidth for sequential profiles, IOPS for random."""
    return {
        "vol_seqwrite_ratio": ratio(remote["seqwrite"].bw_bytes, local["seqwrite"].bw_bytes),
        "vol_seqread_ratio": ratio(remote["seqread"].bw_bytes, local["seqread"].bw_bytes),
        "vol_randwrite_ratio": ratio(remote["randwrite"].iops, local["randwrite"].iops),
        "vol_randread_ratio": ratio(remote["randread"].iops, local["randread"].iops),
    }


def fsync_allowance_us(local_p99_us, rtt_us):
    """§5 budget: fsync p99 may be at most 2x the local zvol's plus one RTT."""
    return 2 * local_p99_us + rtt_us


def mib_per_s(nbytes, seconds):
    if seconds <= 0:
        raise ValueError("elapsed time must be positive")
    return nbytes / (1024 * 1024) / seconds


def ping_max_rtt_us(text):
    for line in text.splitlines():
        if line.startswith("rtt ") and "=" in line:
            return round(float(line.split("=")[1].split("/")[2]) * 1000)
    raise ValueError(f"no rtt summary in ping output: {text[:200]!r}")


def budget(budgets, name):
    for b in budgets:
        if b["name"] == name:
            return b
    raise KeyError(f"no budget named {name}")


def check_budgets(measured, budgets):
    """Return one message per violated floor/ceiling among the measured names."""
    msgs = []
    for name, value in measured.items():
        b = budget(budgets, name)
        if b.get("max") and value > b["max"]:
            msgs.append(f"{name} = {value:g} exceeds ceiling {b['max']:g}")
        if b.get("min") and value < b["min"]:
            msgs.append(f"{name} = {value:g} is below floor {b['min']:g}")
    return msgs


def pass_metric(kind, result):
    """What a first-touch pass is judged by: bandwidth for a sequential pass, IOPS for a random one."""
    return {"seq": result.bw_bytes, "rand": result.iops}[kind]


def thin_axis(cells):
    """The thin-vs-thick comparisons D2 turns on, from one metric measured in five cells."""
    return {
        "local_first_touch": ratio(cells["local-thin-cold"], cells["local-thin-warm"]),
        "local_thin_vs_thick": ratio(cells["local-thin-warm"], cells["local-thick"]),
        "vol_first_touch": ratio(cells["vol-cold"], cells["vol-warm"]),
        "vol_cold_vs_local_thick": ratio(cells["vol-cold"], cells["local-thick"]),
    }


def vmstat_mean(text):
    """Mean CPU percentages of `vmstat` output, without its first line of samples (the average since boot)."""
    lines = [line.split() for line in text.splitlines() if line.strip()]
    header = next((cols for cols in lines if "us" in cols), None)
    rows = [cols for cols in lines if cols[0].isdigit()][1:]
    if header is None or not rows:
        raise ValueError(f"no vmstat samples in: {text[:200]!r}")
    return {c: sum(float(r[header.index(c)]) for r in rows) / len(rows) for c in ("us", "sy", "id", "wa", "st")}


def iperf_mib_s(text):
    """Received MiB/s of an `iperf3 --json` run."""
    report = json.loads(text)
    if "error" in report:
        raise ValueError(f"iperf3 failed: {report['error']}")
    return report["end"]["sum_received"]["bits_per_second"] / 8 / (1024 * 1024)


def enforce(measured, budgets):
    """Split budget violations into (problems, waived); a budget's vm_waiver says why the VM cannot judge it."""
    problems, waived = [], []
    for name, value in measured.items():
        for msg in check_budgets({name: value}, budgets):
            reason = budget(budgets, name).get("vm_waiver")
            (waived if reason else problems).append(f"{msg} (waived: {reason})" if reason else msg)
    return problems, waived
