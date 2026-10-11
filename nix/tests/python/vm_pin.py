"""Pins each test VM to whole physical cores (with their hyperthread siblings).

Spliced into a testScript; the caller passes each machine's slot and its cores per VM.
"""
import itertools
import os


def read(path):
    with open(path) as f:
        return f.read()


def cores_from_cpuinfo(text, allowed):
    """Lists of CPUs per physical core, grouped by socket; drops cores with a CPU not in allowed."""
    cores = {}
    for block in text.split("\n\n"):
        fields = dict(line.split(":", 1) for line in block.splitlines() if ":" in line)
        fields = {k.strip(): v.strip() for k, v in fields.items()}
        if "processor" not in fields:
            continue
        cpu = int(fields["processor"])
        # Without topology (some VMs), each CPU stands alone.
        key = (int(fields.get("physical id", 0)), int(fields.get("core id", cpu)))
        cores.setdefault(key, []).append(cpu)
    return [sorted(c) for _, c in sorted(cores.items()) if set(c) <= allowed]


def physical_cores():
    """The host's physical cores; /proc/cpuinfo, since the build sandbox has no /sys."""
    return cores_from_cpuinfo(read("/proc/cpuinfo"), os.sched_getaffinity(0))


CORES = physical_cores()


def descendants(pid):
    kids = {}
    for p in os.listdir("/proc"):
        if p.isdigit():
            try:
                kids.setdefault(int(read(f"/proc/{p}/stat").rsplit(")", 1)[1].split()[1]), []).append(int(p))
            except (OSError, IndexError):
                pass
    out, todo = [], [pid]
    while todo:
        p = todo.pop()
        out.append(p)
        todo.extend(kids.get(p, []))
    return out


def pin(m, slot, per_vm):
    """Pins every thread of m's QEMU to physical cores slot*per_vm onward; skips when the host is too small."""
    first = slot * per_vm
    if len(CORES) < first + per_vm:
        print(f"PIN {m.name}: only {len(CORES)} cores available, left unpinned")
        return
    cpus = set(itertools.chain.from_iterable(CORES[first:first + per_vm]))
    for pid in descendants(m.process.pid):
        for tid in os.listdir(f"/proc/{pid}/task"):
            try:
                os.sched_setaffinity(int(tid), cpus)
            except OSError:
                pass
    print(f"PIN {m.name} -> CPUs {sorted(cpus)}")

