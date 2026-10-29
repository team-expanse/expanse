"""vm-live-resize (Phase 16 V4): a redeploy with new CPU and memory requests hot-adds or removes
vCPUs and virtio-mem memory in the running guest, up to the block's limits, with no reboot; the
new size survives a suspend and resume.

Runs after cluster-common.py, block-common.py and vol_cluster.py, with KERNEL, INITRD, CMDLINE
and GUEST_IP (vm-instance.nix) spliced in ahead of this file.
"""

import shlex

NAME = "vm1"
UNIT = f"expanse-block-root@default-{NAME}-0.service"
SW = "/run/current-system/sw/bin"
ALL = {"n1": n1, "n2": n2, "n3": n3}


def manifest(cpu, memory):
    return f"""apiVersion: expanse.io/v1
kind: Block
metadata:
  name: {NAME}
  namespace: default
spec:
  type: vm/instance
  replicas: 1
  strategy:
    kind: SINGLETON
  resources:
    requests:
      cpu: "{cpu}"
      memory: {memory}
    limits:
      cpu: "4"
      memory: 3Gi
  storage:
    - name: disk
      size: 16Mi
      replication: 3
      mountPath: /mnt/{NAME}-disk
      filesystem: none
  placement:
    requiredCapabilities: [kvm]
  config:
    bootKernel: "{KERNEL}"
    bootInitrd: "{INITRD}"
    bootCmdline: "{CMDLINE}"
"""


def guest(m, script):
    """Run script in the guest through its guest agent, from the VM's node m."""
    return m.succeed(f"expanse ctl vm exec {NAME} -- {SW}/sh -c {shlex.quote(script)}").strip()


def size(m):
    """The guest's online vCPUs and MemTotal in MiB."""
    cpus, kib = guest(m, "nproc; awk '/^MemTotal:/ {print $2}' /proc/meminfo").split()
    return int(cpus), int(kib) // 1024


def uptime(m):
    return float(guest(m, "cut -d' ' -f1 /proc/uptime"))


def qemu_pid(m):
    return m.succeed(f"pgrep -P $(systemctl show -P MainPID {UNIT}) qemu").strip()


def qemu_argv(m):
    return m.succeed(f"tr '\\0' ' ' < /proc/{qemu_pid(m)}/cmdline")


def cursor(m):
    return m.succeed("journalctl --show-cursor -n0 | tail -1 | cut -d' ' -f3").strip()


def journal_since(m, cur):
    return m.succeed(f"journalctl --after-cursor='{cur}' -u {UNIT} --no-pager")


def resize_live(m, cpu, memory, want_cpus, mem_ok):
    """Redeploy with new requests and wait for the guest to reach them, on the same qemu and boot."""
    pid, before, cur = qemu_pid(m), uptime(m), cursor(m)
    deploy(n1, NAME, manifest(cpu, memory))
    got = {}

    def reached():
        try:  # the agent may miss a call while vCPUs come and go
            got["size"] = size(m)
        except Exception:
            return False
        print(f"guest size: {got['size']}")
        return got["size"][0] == want_cpus and mem_ok(got["size"][1])

    wait_for(reached, f"{NAME} at {want_cpus} vCPUs and {memory}", timeout=180)
    assert qemu_pid(m) == pid, "qemu restarted for a live resize"
    assert uptime(m) >= before, "the guest rebooted"
    assert guest(m, "cat /run/marker") == MARK, "the guest's tmpfs did not survive"
    journal = journal_since(m, cur)
    assert "applied its new spec live" in journal, journal
    assert "restarting it" not in journal, journal
    return got["size"]


form("vmresize")
for m in ALL.values():
    wait_agent_ready(m)

with subtest("a VM boots with hotplug room up to its limits"):
    deploy(n1, NAME, manifest("1", "2Gi"))
    wait_phase(n1, NAME, ["RUNNING"], 300)
    nodes = placement_nodes(get_json(n1, NAME) or {})
    assert len(nodes) == 1, f"{NAME} placed on {nodes}"
    host_name = next(iter(nodes))
    host = ALL[host_name]
    host.wait_until_succeeds(f"expanse ctl vm exec {NAME} -- {SW}/true", timeout=180)
    argv = qemu_argv(host)
    assert "1,maxcpus=4,sockets=1,cores=4,threads=1" in argv and "-m 2048M,maxmem=3072M " in argv, argv
    assert "virtio-mem-pci,id=vmem0dev,memdev=vmem0,requested-size=0M" in argv, argv
    MARK = guest(host, "head -c 8 /dev/urandom | od -An -tx1 | tr -d ' \\n' | tee /run/marker")
    cpus, base_mib = size(host)
    assert cpus == 1, f"{cpus} vCPUs at boot"
    print(f"boot: {cpus} vCPUs, MemTotal {base_mib} MiB")

with subtest("a grow adds vCPUs and memory to the running guest"):
    cpus, mib = resize_live(host, "3", "2560Mi", 3, lambda mib: mib >= base_mib + 500)
    print(f"grown: {cpus} vCPUs, MemTotal {mib} MiB")

with subtest("a shrink takes them back from the running guest"):
    cpus, mib = resize_live(host, "1", "2Gi", 1, lambda mib: mib <= base_mib + 16)
    print(f"shrunk: {cpus} vCPUs, MemTotal {mib} MiB")

with subtest("a resized VM keeps its size through a suspend and resume"):
    resize_live(host, "2", "2560Mi", 2, lambda mib: mib >= base_mib + 500)
    before, cur = uptime(host), cursor(host)
    n1.succeed(f"expanse ctl vm suspend {NAME}")
    wait_phase(n1, NAME, ["SUSPENDED"], 300)
    n1.succeed(f"expanse ctl vm resume {NAME}")
    wait_phase(n1, NAME, ["RUNNING"], 300)
    journal = journal_since(host, cur)
    assert "resumed from its saved state" in journal, journal
    cpus, mib = size(host)
    assert cpus == 2 and mib >= base_mib + 500, f"resumed at {cpus} vCPUs, {mib} MiB"
    assert uptime(host) >= before, "the guest rebooted"
    assert guest(host, "cat /run/marker") == MARK, "the guest's tmpfs did not survive"

print("VM-LIVE-RESIZE DONE")
