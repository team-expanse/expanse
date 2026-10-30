"""vm-upgrade-keep (Phase 16 V5): a switch to a release with another QEMU flags the running VM;
`expanse ctl vm update` moves it onto that QEMU, and a node reboot saves it and the next boot
resumes it. The guest's uptime and a file in its tmpfs survive both.

Runs after cluster-common.py, block-common.py and vol_cluster.py, with KERNEL, INITRD, CMDLINE
and GUEST_IP (vm-instance.nix) spliced in ahead of this file.
"""

import shlex

NAME = "vm1"
UNIT = f"expanse-block-root@default-{NAME}-0.service"
SW = "/run/current-system/sw/bin"
NOTE = "QEMU update pending"

MANIFEST = f"""apiVersion: expanse.io/v1
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
      cpu: 1000m
      memory: 2Gi
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

ALL = {"n1": n1, "n2": n2, "n3": n3}


def guest(m, script):
    """Run script in the guest through its guest agent, from the VM's node m."""
    return m.succeed(f"expanse ctl vm exec {NAME} -- {SW}/sh -c {shlex.quote(script)}").strip()


def uptime(m):
    return float(guest(m, "cut -d' ' -f1 /proc/uptime"))


def host_of(m):
    nodes = placement_nodes(get_json(m, NAME) or {})
    assert len(nodes) == 1, f"{NAME} placed on {nodes}"
    name = next(iter(nodes))
    return name, ALL[name]


def cursor(m):
    return m.succeed("journalctl --show-cursor -n0 | tail -1 | cut -d' ' -f3").strip()


def journal_since(m, cur):
    return m.succeed(f"journalctl --after-cursor='{cur}' -u {UNIT} --no-pager")


def status_text(m):
    return m.succeed(f"systemctl show -p StatusText --value {UNIT}").strip()


def keeps_guest(m, mark, before):
    m.wait_until_succeeds(f"expanse ctl vm exec {NAME} -- {SW}/true", timeout=300)
    assert guest(m, "cat /run/marker") == mark, "the guest's tmpfs did not survive"
    after = uptime(m)
    assert after >= before, f"uptime went from {before} to {after}: the guest rebooted"


form("vmup")
for m in ALL.values():
    wait_agent_ready(m)

with subtest("a running VM holds a marker in its tmpfs"):
    deploy(n1, NAME, MANIFEST)
    wait_phase(n1, NAME, ["RUNNING"], 300)
    host_name, host = host_of(n1)
    other = next(m for n, m in ALL.items() if n != host_name)
    host.wait_until_succeeds(f"expanse ctl vm exec {NAME} -- {SW}/true", timeout=180)
    MARK = guest(host, "head -c 8 /dev/urandom | od -An -tx1 | tr -d ' \\n' | tee /run/marker")
    assert len(MARK) == 16, f"marker {MARK!r}"
    assert NOTE not in status_text(host), status_text(host)

with subtest("a switch to another QEMU leaves the VM running and flags it"):
    before = uptime(host)
    host.succeed("/run/current-system/specialisation/newqemu/bin/switch-to-configuration test")
    host.wait_until_succeeds(f"systemctl show -p StatusText --value {UNIT} | grep -q '{NOTE}'", timeout=240)
    keeps_guest(host, MARK, before)
    assert "qemu-kvm-next" not in host.succeed(f"systemctl status --no-pager -l {UNIT}")

with subtest("update moves the VM onto the new QEMU and keeps its guest"):
    before = uptime(host)
    cur = cursor(host)
    other.succeed(f"expanse ctl vm update {NAME}")
    host.wait_until_succeeds(
        f"journalctl --after-cursor='{cur}' -u {UNIT} --no-pager | grep -q 'resumed from its saved state'", timeout=300)
    journal = journal_since(host, cur)
    assert "saved for an update" in journal, journal
    assert "reboot: Power down" not in journal, journal
    keeps_guest(host, MARK, before)
    host.wait_until_succeeds(f"! systemctl show -p StatusText --value {UNIT} | grep -q '{NOTE}'", timeout=120)
    assert "qemu-kvm-next" in host.succeed(f"systemctl status --no-pager -l {UNIT}")
    assert host_of(other)[0] == host_name, "the updated VM moved"

with subtest("a node reboot saves the VM, keeps it placed, and the next boot resumes it"):
    before = uptime(host)
    host.shutdown()
    time.sleep(45)  # past the 30 s unreachable grace: a reboot announcement must hold the placement
    assert host_of(other)[0] == host_name, "the VM moved while its node rebooted"
    host.start()
    host.wait_for_unit("expansed.service", timeout=600)
    wait_agent_ready(host)
    keeps_guest(host, MARK, before)
    assert host_of(other)[0] == host_name, "the VM moved"
    host.succeed(f"journalctl -b -1 -u {UNIT} --no-pager | grep -q \"saved for the node's shutdown\"")
    host.succeed(f"journalctl -b 0 -u {UNIT} --no-pager | grep -q 'resumed from its saved state'")
    host.succeed(f"journalctl -b -1 -u expansed --no-pager | grep -q 'agent stopping'")

print("VM-UPGRADE-KEEP DONE")
