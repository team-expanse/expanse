"""vol-perf (E5, X2): the storage perf budgets on the real stack, and the thin-vs-thick axis that settles D2.

Two replication-3 volumes are created through the agent (thin, as production runs). On the node that is their
primary, local LVs of the same size are made in the same pool (thin) and outside it (thick). Then:

  first touch  each device gets one pass while never written, then the same pass again. Sequential and random
               use separate devices so neither warms the other. Thin cold vs warm, replicated cold vs warm,
               and cold vs a thick local LV are the D2 numbers.
  budgets      the warm suite (4 fio profiles and an fsync latency) on the replicated volume against the same on
               a local thin LV, checked against test/perf/budgets.yaml. A thick suite is measured for the record.
  resync       a full resync of one replica, timed, then again with csums-alg set to see what it would save.
  failover     the primary is hard-killed three times; kill to next successful write, judged on the median.

Runs after cluster-common.py, vol_cluster.py, vol_perf_lib.py, BUDGETS and vol_perf_fio.py.
"""

import statistics

SIZE_MIB = 1024
AXIS_MIB = 256
VG = "vg0"
FAILOVER_ROUNDS = 3
CSUMS_ALG = "sha1"
READ_ROUNDS = 3
CHUNK_POOL_MIB = 3584


def lv_thin(m, name, size_mib=SIZE_MIB):
    m.succeed(f"lvcreate --yes --type thin --thinpool {VG}/pool --virtualsize {size_mib}M --name {name} {VG}")
    return f"/dev/{VG}/{name}"


def lv_thick(m, name):
    m.succeed(f"lvcreate --yes --size {SIZE_MIB}M --name {name} {VG}")
    return f"/dev/{VG}/{name}"


def device_for(m, res):
    return m.succeed(f"drbdadm sh-dev {res}").strip()


def mesh_peer_ips(m, res):
    """The mesh addresses of m's peers, from the resource's own config."""
    mine = re.findall(r"inet (\d+\.\d+\.\d+\.\d+)/", m.succeed("ip -4 -o addr show dev exp0"))
    return [ip for ip in re.findall(r"address\s+(?:ipv4\s+)?(\d+\.\d+\.\d+\.\d+):", m.succeed(f"drbdadm dump {res}")) if ip not in mine]


def worst_rtt_us(m, res):
    return max(ping_max_rtt_us(m.succeed(f"ping -c 20 -i 0.2 -q {ip}")) for ip in mesh_peer_ips(m, res))


def rx_bytes(m):
    """Bytes received on the mesh interface, which carries all DRBD traffic."""
    for line in m.succeed("cat /proc/net/dev").splitlines():
        if line.strip().startswith("exp0:"):
            return int(line.split(":", 1)[1].split()[0])
    raise Exception(f"no exp0 on {m.name}")


def mesh_ip(m):
    return re.findall(r"inet (\d+\.\d+\.\d+\.\d+)/", m.succeed("ip -4 -o addr show dev exp0"))[0]


def fanout_mib_s(src, peer_ips):
    """Total MiB/s src can push to all peers at once, which is what a write to every replica needs."""
    for peer in [m for m in NODES if m is not src]:
        peer.succeed("pgrep iperf3 || iperf3 -s -D")
    jobs = "".join(f"iperf3 -c {ip} -t 5 -J > /tmp/iperf-{i}.json & " for i, ip in enumerate(peer_ips))
    src.succeed(jobs + "wait")
    return sum(iperf_mib_s(src.succeed(f"cat /tmp/iperf-{i}.json")) for i in range(len(peer_ips)))


def fio_watching_cpu(dev, node, profile):
    """One profile on node's dev while every machine samples its CPU; returns (result, {machine: mean CPU %})."""
    for m in NODES:
        m.succeed(f"vmstat 1 {RUNTIME_S + RAMP_S + 1} > /tmp/vmstat.txt &")
    result = fio(node, dev, profile)
    time.sleep(3)
    return result, {m.name: vmstat_mean(m.succeed("cat /tmp/vmstat.txt")) for m in NODES}


def idle_cpu_report():
    """What an idle node costs with these volumes: CPU per machine over 10 s, and n2's busiest processes."""
    for m in NODES:
        m.succeed("vmstat 1 11 > /tmp/idle.txt &")
    time.sleep(13)
    for m in NODES:
        cpu = vmstat_mean(m.succeed("cat /tmp/idle.txt"))
        print(f"idle {m.name}: us+sy {cpu['us'] + cpu['sy']:.0f}% wa {cpu['wa']:.0f}%")
    print("idle n2 busiest now:\n" + n2.succeed("top -b -n 2 -d 5 -o %CPU | awk '/^top -/{n++} n==2' | sed -n '7,14p'"))


def median_read_ratios(node, local_dev, remote_dev, first):
    """Read ratios as the median of READ_ROUNDS alternating runs: one run moves by 10% or more between test runs."""
    kinds = {"vol_seqread_ratio": ("seqread", "seq"), "vol_randread_ratio": ("randread", "rand")}
    seen = {name: [first[name]] for name in kinds}
    for _ in range(READ_ROUNDS - 1):
        for name, (profile, kind) in kinds.items():
            local, remote = fio(node, local_dev, profile), fio(node, remote_dev, profile)
            seen[name].append(ratio(pass_metric(kind, remote), pass_metric(kind, local)))
    return {name: statistics.median(values) for name, values in seen.items()}


def healthy(name):
    return (volume_row(n1, name) or {}).get("state") == "healthy"


def create_replicated(name, replication, size_mib=SIZE_MIB):
    n1.succeed(f"expanse ctl volume create {name} --size {size_mib}Mi --replication {replication}")


def synced_with(m, res, peers):
    text = drbd_status(m, res)
    return "disk:UpToDate" in text.split("\n")[0:2][-1] and text.count("peer-disk:UpToDate") == peers


def replicated_id(name, replication):
    wait_for(lambda: healthy(name), f"{name} to be Healthy", 180)
    res = volume_row(n1, name)["id"]
    wait_for(lambda: len(primaries(res)) == 1, f"one primary of {name}")
    wait_for(lambda: synced_with(primaries(res)[0], res, replication - 1), f"{name}'s replicas to be UpToDate", 180)
    return res


def first_and_second(m, dev, kind):
    """(cold, warm) metric of two identical passes over dev."""
    cold = pass_metric(kind, one_pass(m, dev, kind))
    return cold, pass_metric(kind, one_pass(m, dev, kind))


def wait_replicated_on(res, timeout=300):
    wait_for(lambda: len(primaries(res)) == 1 and fully_replicated(primaries(res)[0], res), "every replica to be UpToDate", timeout)


def resync_measured(primary, victim, res, prepare=lambda: None):
    """Invalidate victim's copy and time the copy back; returns (seconds, bytes the victim received)."""
    prepare()
    before, start = rx_bytes(victim), time.time()
    victim.succeed(f"drbdadm invalidate {res}")
    wait_for(lambda: not fully_replicated(primary, res), "the invalidation to reach the primary", 30)
    wait_replicated_on(res)
    return time.time() - start, rx_bytes(victim) - before


def set_csums(victim, res):
    """csums-alg on every connection of victim, which is the side that requests the blocks."""
    status = json.loads(victim.succeed(f"drbdsetup status {res} --json"))[0]
    for conn in status["connections"]:
        victim.succeed(f"drbdsetup net-options {res} {conn['peer-node-id']} --csums-alg={CSUMS_ALG}")


def crash_and_time_failover(res):
    """Hard-kill the primary; returns (killed node, seconds until another node accepts a write)."""
    doomed = primaries(res)[0]
    survivors = [m for m in NODES if m is not doomed]
    started = time.time()
    doomed.crash()
    while time.time() < started + 180:
        for m in survivors:
            if m.execute(f"dd if=/dev/urandom of={device_for(m, res)} bs=4k count=1 oflag=direct conv=notrunc 2>/dev/null")[0] == 0:
                return doomed, time.time() - started
        time.sleep(0.2)
    raise AssertionError("no node accepted a write within 180s of the primary crash")


def restore(dead, res):
    dead.start()
    dead.wait_for_unit("multi-user.target", timeout=180)
    dead.wait_for_unit("expansed.service", timeout=120)
    wait_agent_ready(dead)
    wait_replicated_on(res, 300)


def dump_on_failure():
    for m in NODES:
        print(f"[{m.name}] volumes:\n{m.execute('expanse ctl volume list 2>&1')[1]}")
        print(f"[{m.name}] lvs:\n{m.execute('lvs 2>&1')[1]}")
        print(f"[{m.name}] agent:\n{m.execute('journalctl -u expansed.service -n 15 --no-pager 2>&1')[1]}")


form("vperf")
problems = []
measured = {}

try:
    with subtest("the mesh's own throughput, to one peer set at once: what replication can be given"):
        mesh_ips = [mesh_ip(m) for m in NODES if m is not n1]
        for ip in mesh_ips:
            wait_for(lambda: n1.execute(f"ping -c 1 -W 1 {ip}")[0] == 0, f"the mesh to carry traffic to {ip}", 90)
        mesh = fanout_mib_s(n1, mesh_ips)
        lan = fanout_mib_s(n1, [f"192.168.1.{i}" for i in (2, 3)])
        print(f"n1 to n2 and n3 at once: mesh {mesh:.0f} MiB/s, LAN {lan:.0f} MiB/s")

    with subtest("two replication-3 volumes are created thin through the agent"):
        for name, replication, size_mib in (("vseq", 3, SIZE_MIB), ("vrand", 3, SIZE_MIB), ("vr1", 1, AXIS_MIB), ("vr2", 2, AXIS_MIB), ("vr3", 3, AXIS_MIB)):
            create_replicated(name, replication, size_mib)
        res_seq, res_rand = replicated_id("vseq", 3), replicated_id("vrand", 3)
        axis_res = {"R1": replicated_id("vr1", 1), "R2": replicated_id("vr2", 2), "R3": replicated_id("vr3", 3)}
        node = primaries(res_seq)[0]
        for res in [res_rand, *axis_res.values()]:
            assert primaries(res) == [node], f"{res} is primary on {primaries(res)}, not {node.name}"
        assert node.succeed(f"lvs --noheadings -o lv_layout {VG}/{res_seq}").strip() == "thin,sparse", "the volume is not thin"
        dev_seq, dev_rand = device_for(node, res_seq), device_for(node, res_rand)
        print(f"{node.name} is primary of both; devices {dev_seq}, {dev_rand}")

    with subtest("local baselines: thin and thick LVs of the same size on the primary"):
        local = {"thin-seq": lv_thin(node, "thin_seq"), "thin-rand": lv_thin(node, "thin_rand"),
                 "thick-seq": lv_thick(node, "thick_seq"), "thick-rand": lv_thick(node, "thick_rand"), "axis": lv_thin(node, "thin_axis", AXIS_MIB)}

    with subtest("the same writes at 1, 2 and 3 replicas, with the CPU each machine spent"):
        idle_cpu_report()
        devices = {"local": local["axis"], **{label: device_for(node, res) for label, res in axis_res.items()}}
        for dev in devices.values():
            one_pass(node, dev, "seq")
        rates = {}
        for profile in ("seqwrite", "randwrite"):
            for label, dev in devices.items():
                result, cpu = fio_watching_cpu(dev, node, profile)
                rates[profile, label] = pass_metric("seq" if profile == "seqwrite" else "rand", result)
                busiest = ", ".join(f"{name} us+sy {c['us'] + c['sy']:.0f}% wa {c['wa']:.0f}% st {c['st']:.0f}%" for name, c in cpu.items())
                print(f"axis {profile} {label}: {result.bw_bytes / 1048576:.1f} MiB/s, {result.iops:.0f} iops; cpu: {busiest}")
        measured.update({
            "vol_r1_seqwrite_ratio": ratio(rates["seqwrite", "R1"], rates["seqwrite", "local"]),
            "vol_r1_randwrite_ratio": ratio(rates["randwrite", "R1"], rates["randwrite", "local"]),
            "vol_r3_seqwrite_of_r2_ratio": ratio(rates["seqwrite", "R3"], rates["seqwrite", "R2"]),
            "vol_r3_randwrite_of_r2_ratio": ratio(rates["randwrite", "R3"], rates["randwrite", "R2"]),
        })

    with subtest("first touch: a device never written, then the same pass again (D2)"):
        axis = {}
        for kind, unit in (("seq", "MiB/s"), ("rand", "iops")):
            scale = 1048576 if kind == "seq" else 1
            thin_cold, thin_warm = first_and_second(node, local[f"thin-{kind}"], kind)
            thick_first, thick = first_and_second(node, local[f"thick-{kind}"], kind)
            vol_cold, vol_warm = first_and_second(node, dev_seq if kind == "seq" else dev_rand, kind)
            cells = {"local-thin-cold": thin_cold, "local-thin-warm": thin_warm, "local-thick": thick,
                     "local-thick-first": thick_first, "vol-cold": vol_cold, "vol-warm": vol_warm}
            axis[kind] = thin_axis(cells)
            print(f"first touch {kind} ({unit}): " + ", ".join(f"{k} {v / scale:.1f}" for k, v in cells.items()))
            print(f"first touch {kind} ratios: " + ", ".join(f"{k} {v:.2f}" for k, v in axis[kind].items()))
        measured["vol_first_touch_ratio"] = min(a["vol_first_touch"] for a in axis.values())

    with subtest("thin chunk size: a 1 MiB chunk against the pool's default, sequential and random"):
        node.succeed(f"lvcreate --yes --type thin-pool --chunksize 1M -L {CHUNK_POOL_MIB}M -n pool1m {VG}")
        for kind in ("seq", "rand"):
            node.succeed(f"lvcreate --yes --type thin --thinpool {VG}/pool1m --virtualsize {SIZE_MIB}M --name chunk_{kind} {VG}")
        cold_rand, warm_rand = first_and_second(node, f"/dev/{VG}/chunk_rand", "rand")
        first_and_second(node, f"/dev/{VG}/chunk_seq", "seq")
        big = measure_all(node, f"/dev/{VG}/chunk_seq")
        print_suite("local thin 1M chunk", big)
        print(f"1M chunk random writes: cold {cold_rand:.0f} iops, warm {warm_rand:.0f} iops, cold is {cold_rand / warm_rand:.2f} of warm "
              f"(default chunk: {axis['rand']['local_first_touch']:.2f})")
        # a snapshot makes the first write to each chunk copy the whole chunk: the price of a big chunk
        for label, lv in (("64K", "thin_rand"), ("1M", "chunk_rand")):
            node.succeed(f"lvcreate --yes --snapshot --name snap_{lv} {VG}/{lv}")
            shared = pass_metric("rand", one_pass(node, f"/dev/{VG}/{lv}", "rand"))
            print(f"{label} chunk random writes into a snapshotted volume: {shared:.0f} iops")

    with subtest("budgets: the replicated volume against a local thin LV, both written"):
        wait_replicated_on(res_seq)
        local_thin, remote = measure_all(node, local["thin-seq"]), measure_all(node, dev_seq)
        print_suite("local thin", local_thin)
        print_suite("replicated", remote)
        rtt_us = worst_rtt_us(node, res_seq)
        found, extra = compare_suites(local_thin, remote, rtt_us)
        measured.update(found)
        measured.update(median_read_ratios(node, local["thin-seq"], dev_seq, found))
        problems += extra
        print_suite("local thick", measure_all(node, local["thick-seq"]))

    with subtest("a full resync of one replica, then again with csums-alg"):
        victim = [m for m in NODES if m is not node][0]
        plain_s, plain_bytes = resync_measured(node, victim, res_seq)
        rate = mib_per_s(SIZE_MIB * 1024 * 1024, plain_s)
        print(f"full resync: {plain_bytes / 1048576:.0f} MiB received in {plain_s:.0f}s = {rate:.1f} MiB/s")
        measured["vol_resync_mbps"] = rate
        # the agent would adjust a net option the config file does not carry away again mid-resync
        victim.succeed("systemctl stop expansed.service")
        csums_s, csums_bytes = resync_measured(node, victim, res_seq, lambda: set_csums(victim, res_seq))
        print(f"full resync with csums-alg={CSUMS_ALG}: {csums_bytes / 1048576:.0f} MiB received in {csums_s:.0f}s")
        victim.succeed("systemctl start expansed.service")
        wait_agent_ready(victim)
        assert csums_bytes < plain_bytes / 2, f"csums-alg saved nothing: {csums_bytes} of {plain_bytes} bytes"

    with subtest(f"failover: {FAILOVER_ROUNDS} hard kills of the primary, kill to next successful write"):
        times = []
        for round_no in range(FAILOVER_ROUNDS):
            wait_replicated_on(res_seq)
            doomed, seconds = crash_and_time_failover(res_seq)
            times.append(seconds * 1000)
            print(f"failover {round_no + 1}: {doomed.name} killed, next write accepted after {seconds:.1f}s")
            restore(doomed, res_seq)
        median_ms = statistics.median(times)
        print(f"failover median {median_ms:.0f} ms of {[round(t) for t in times]}")
        measured.update({"vol_failover_ms": median_ms, "vol_failover_worst_ms": max(times)})
except Exception:
    dump_on_failure()
    raise

budget_problems, waived = enforce(measured, BUDGETS)
print("measured: " + ", ".join(f"{k} {v:.3g}" for k, v in sorted(measured.items())))
for line in waived:
    print("WAIVED " + line)
assert not budget_problems + problems, "perf budgets violated: " + "; ".join(budget_problems + problems)
print(f"VOL-PERF PASSED: {len(measured) - len(waived)} budgets met, {len(waived)} waived (see budgets.yaml)")
