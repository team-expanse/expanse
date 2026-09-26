"""vol-constrained (E7, X8): the whole-node budget (ARCHITECTURE.md §8) holds on the
advertised minimum — 4 GB RAM, 2 cores, one data disk per node.

Unlike vol-perf (which measures throughput ratios on looser VMs for speed), this test pins
every node at the real target and checks the things that budget is actually about: a
replicated volume still goes healthy, the agent's own idle overhead with that volume attached
stays under node_control_plane_rss_bytes/node_control_plane_cpu_percent, and a hard-killed
primary still fails over inside vol_failover_worst_ms.

Runs after cluster-common.py, vol_cluster.py, vol_perf_lib.py, node_overhead.py and BUDGETS.
"""

SIZE_MIB = 512


def healthy(name):
    return (volume_row(n1, name) or {}).get("state") == "healthy"


def create_replicated(name, replication=3, size_mib=SIZE_MIB):
    n1.succeed(f"expanse ctl volume create {name} --size {size_mib}Mi --replication {replication}")


def replicated_id(name, replication):
    wait_for(lambda: healthy(name), f"{name} to be Healthy", 180)
    res = volume_row(n1, name)["id"]
    wait_for(lambda: len(primaries(res)) == 1, f"one primary of {name}")
    wait_for(lambda: fully_replicated(primaries(res)[0], res), f"{name}'s replicas to be UpToDate", 180)
    return res


def crash_and_time_failover(res):
    """Hard-kill the primary; returns (killed node, seconds until another node accepts a write)."""
    doomed = primaries(res)[0]
    survivors = [m for m in NODES if m is not doomed]
    started = time.time()
    doomed.crash()
    while time.time() < started + 180:
        for m in survivors:
            dev = m.succeed(f"drbdadm sh-dev {res}").strip()
            if m.execute(f"dd if=/dev/urandom of={dev} bs=4k count=1 oflag=direct conv=notrunc 2>/dev/null")[0] == 0:
                return doomed, time.time() - started
        time.sleep(0.2)
    raise AssertionError("no node accepted a write within 180s of the primary crash")


def restore(dead, res):
    dead.start()
    dead.wait_for_unit("multi-user.target", timeout=180)
    dead.wait_for_unit("expansed.service", timeout=120)
    wait_agent_ready(dead)
    wait_for(lambda: len(primaries(res)) == 1 and fully_replicated(primaries(res)[0], res), "replicas UpToDate after restore", 300)


def dump_on_failure():
    for m in NODES:
        print(f"[{m.name}] volumes:\n{m.execute('expanse ctl volume list 2>&1')[1]}")
        print(f"[{m.name}] lvs:\n{m.execute('lvs 2>&1')[1]}")
        print(f"[{m.name}] agent:\n{m.execute('journalctl -u expansed.service -n 15 --no-pager 2>&1')[1]}")


form_start = time.time()
form("test")
print(f"cluster formed in {time.time() - form_start:.1f}s on 4 GB RAM / 2 cores")

measured = {}

try:
    with subtest("a replicated volume goes healthy on the constrained node"):
        create_replicated("cvol")
        res = replicated_id("cvol", 3)
        node = primaries(res)[0]
        print(f"cvol healthy, primary {node.name}")

    with subtest("the agent's own idle overhead stays in budget with the volume attached"):
        rss, cpu = idle_control_plane_overhead(node)
        measured["node_control_plane_rss_bytes"] = rss
        measured["node_control_plane_cpu_percent"] = cpu
        print(f"{node.name} idle overhead with cvol active: {rss / 1048576:.1f} MiB RSS, {cpu:.2f}% of one core")

    with subtest("a hard-killed primary fails over inside budget"):
        doomed, seconds = crash_and_time_failover(res)
        measured["vol_failover_worst_ms"] = seconds * 1000
        print(f"{doomed.name} killed, next write accepted after {seconds:.1f}s")
        restore(doomed, res)
except Exception:
    dump_on_failure()
    raise

problems, waived = enforce(measured, BUDGETS)
print("measured: " + ", ".join(f"{k} {v:.3g}" for k, v in sorted(measured.items())))
for line in waived:
    print("WAIVED " + line)
assert not problems, "whole-node budget (ARCHITECTURE.md §8) violated on 4 GB / 2 core / one data disk: " + "; ".join(problems)
print(f"VOL-CONSTRAINED PASSED: {len(measured) - len(waived)} budgets met, {len(waived)} waived, on 4 GB RAM / 2 cores / one data disk")
