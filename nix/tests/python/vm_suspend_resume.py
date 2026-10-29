"""vm-suspend-resume (Phase 16 V2): `expanse ctl vm suspend` saves a running guest's RAM on its
node and stops it; `resume` continues the same boot there. A VM moved while suspended boots fresh,
and its old node drops the save.

Runs after cluster-common.py, block-common.py and vol_cluster.py, with KERNEL, INITRD, CMDLINE
and GUEST_IP (vm-instance.nix) spliced in ahead of this file.
"""

import shlex

NAME = "vm1"
UNIT = f"expanse-block-root@default-{NAME}-0.service"
STATE = f"/persist/expanse/vm-state/default-{NAME}-0"
SW = "/run/current-system/sw/bin"

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


def host_of():
    nodes = placement_nodes(get_json(n1, NAME) or {})
    assert len(nodes) == 1, f"{NAME} placed on {nodes}"
    name = next(iter(nodes))
    return name, ALL[name]


def cursor(m):
    return m.succeed("journalctl --show-cursor -n0 | tail -1 | cut -d' ' -f3").strip()


def journal_since(m, cur):
    return m.succeed(f"journalctl --after-cursor='{cur}' -u {UNIT} --no-pager")


def suspend():
    n1.succeed(f"expanse ctl vm suspend {NAME}")
    return wait_phase(n1, NAME, ["SUSPENDED"], 300)


form("vmsus")
for m in ALL.values():
    wait_agent_ready(m)

with subtest("a running VM holds a marker in its tmpfs"):
    deploy(n1, NAME, MANIFEST)
    wait_phase(n1, NAME, ["RUNNING"], 300)
    host_name, host = host_of()
    host.wait_until_succeeds(f"expanse ctl vm exec {NAME} -- {SW}/true", timeout=180)
    MARK = guest(host, "head -c 8 /dev/urandom | od -An -tx1 | tr -d ' \\n' | tee /run/marker")
    assert len(MARK) == 16, f"marker {MARK!r}"
    before = uptime(host)
    others = [m for n, m in ALL.items() if n != host_name]

with subtest("suspend saves the guest on its node and stops it"):
    cur = cursor(host)
    b = suspend()
    assert placement_nodes(b) == {host_name}, f"the suspended VM moved: {b.get('status')}"
    host.fail(f"systemctl is-active {UNIT}")
    host.succeed(f"test -s {STATE}/state && test -s {STATE}/token")
    token = host.succeed(f"cat {STATE}/token").strip()
    journal = journal_since(host, cur)
    assert f"suspended ({token})" in journal, f"no save logged for {token}:\n{journal}"
    assert "reboot: Power down" not in journal and "suspend failed" not in journal, journal
    others[0].fail(f"ping -c1 -W2 {GUEST_IP}")
    time.sleep(20)  # several controller and reconcile passes: nothing starts it again
    host.fail(f"systemctl is-active {UNIT}")
    assert (get_json(n1, NAME).get("status") or {}).get("phase") == "SUSPENDED"

with subtest("resume continues the same boot on the same node"):
    cur = cursor(host)
    n1.succeed(f"expanse ctl vm resume {NAME}")
    wait_phase(n1, NAME, ["RUNNING"], 300)
    assert host_of()[0] == host_name, "the resumed VM moved"
    assert guest(host, "cat /run/marker") == MARK, "the guest's tmpfs did not survive"
    after = uptime(host)
    assert after >= before, f"uptime went from {before} to {after}: the guest rebooted"
    journal = journal_since(host, cur)
    assert "resumed from its saved state" in journal, journal
    host.fail(f"test -e {STATE}")  # loaded once, then deleted
    others[0].wait_until_succeeds(f"ping -c1 -W2 {GUEST_IP}", timeout=60)

with subtest("a VM moved while suspended boots fresh, and its old node drops the save"):
    suspend()
    host.succeed(f"test -s {STATE}/state")
    others[0].succeed(f"expanse ctl node drain {host_name}")
    wait_for(lambda: placement_nodes(get_json(others[0], NAME) or {}) - {host_name} != set(),
             f"{NAME} to move off {host_name}", timeout=300)
    wait_phase(others[0], NAME, ["SUSPENDED"], 300)
    host.wait_until_fails(f"test -e {STATE}", timeout=120)
    others[0].succeed(f"expanse ctl vm resume {NAME}")
    wait_phase(others[0], NAME, ["RUNNING"], 300)
    new_name = next(iter(placement_nodes(get_json(others[0], NAME))))
    new_host = ALL[new_name]
    assert new_name != host_name, "the VM is still on the drained node"
    new_host.wait_until_succeeds(f"expanse ctl vm exec {NAME} -- {SW}/true", timeout=180)
    assert guest(new_host, "cat /run/marker 2>/dev/null || echo none") == "none", "a moved VM resumed stale RAM"

print("VM-SUSPEND-RESUME DONE")
