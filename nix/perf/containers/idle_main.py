"""X1 container scenario: expansed's idle CPU/RSS on bare metal, no hypervisor.

Answers only the open question behind node_control_plane_cpu_percent's known_gap: is
vol-constrained's 25-27% idle CPU a VM tax or a real cost? Volumes/DRBD are out of scope
here (containers share one kernel, so DRBD resources collide) -- see ./README.md.

Runs after container_adapter.py, cluster-common.py, vol_perf_lib.py, node_overhead.py and BUDGETS.
"""

SETTLE_S = 30
IDLE_BUDGETS = ("node_control_plane_rss_bytes", "node_control_plane_cpu_percent")

form_start = time.time()
form("test")
print(f"cluster formed in {time.time() - form_start:.1f}s on 4 GB RAM / 2 cores")
print(f"settling {SETTLE_S}s before measuring")
time.sleep(SETTLE_S)

per_node = {}
with subtest("each agent's own idle overhead, no volumes, no hypervisor"):
    for m in NODES:
        rss, cpu = idle_control_plane_overhead(m)
        per_node[m.name] = (rss, cpu)
        print(f"{m.name} idle overhead: {rss / 1048576:.1f} MiB RSS, {cpu:.2f}% of one core")

measured = {
    "node_control_plane_rss_bytes": max(rss for rss, _ in per_node.values()),
    "node_control_plane_cpu_percent": max(cpu for _, cpu in per_node.values()),
}
problems, waived = enforce(measured, BUDGETS)
print("measured (worst node): " + ", ".join(f"{k} {v:.3g}" for k, v in sorted(measured.items())))
for line in waived:
    print("OVER BUDGET (known_gap on record) " + line)
for line in problems:
    print("OVER BUDGET " + line)
print("X1 IDLE: " + ("within budget" if not problems and not waived else "over budget") + f" on {', '.join(IDLE_BUDGETS)}")
