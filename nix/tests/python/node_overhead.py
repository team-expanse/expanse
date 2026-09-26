"""expansed's own idle RSS and CPU on one node: ARCHITECTURE.md §8's
node_control_plane_rss_bytes / node_control_plane_cpu_percent.

Spliced ahead of vol_constrained_main.py (VM harness) and nix/perf/containers/idle_main.py
(container harness); needs only a driver-shaped `m` (succeed, name) and `time`.
"""

IDLE_WINDOW_S = 30


def agent_pid(m):
    return m.succeed("systemctl show -p MainPID --value expansed.service").strip()


def proc_cpu_ticks(m, pid):
    """utime+stime (clock ticks) of pid, from /proc/<pid>/stat (fields 14 and 15)."""
    fields = m.succeed(f"cat /proc/{pid}/stat").split()
    return int(fields[13]) + int(fields[14])


def proc_rss_bytes(m, pid):
    for line in m.succeed(f"cat /proc/{pid}/status").splitlines():
        if line.startswith("VmRSS:"):
            return int(line.split()[1]) * 1024
    raise ValueError(f"no VmRSS for pid {pid} on {m.name}")


def idle_control_plane_overhead(m, window_s=IDLE_WINDOW_S):
    """expansed's own RSS and %CPU (of one core) over an idle window: what our software
    costs on top of the kernel's own DRBD/LVM work, which ARCHITECTURE.md §8 budgets
    separately (the kernel side is not ours to shrink)."""
    pid = agent_pid(m)
    hz = int(m.succeed("getconf CLK_TCK").strip())
    before = proc_cpu_ticks(m, pid)
    m.succeed(f"rm -f /tmp/top-h.txt; (top -bH -d {window_s} -n 2 -p {pid} > /tmp/top-h.txt 2>&1 &)")
    time.sleep(window_s + 2)
    after = proc_cpu_ticks(m, pid)
    cpu_percent = 100 * (after - before) / hz / window_s
    print(f"per-thread breakdown on {m.name}:\n" + m.succeed("cat /tmp/top-h.txt"))
    return proc_rss_bytes(m, pid), cpu_percent
