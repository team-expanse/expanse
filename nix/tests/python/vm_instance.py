"""PHASE-06-TASKS.md Stream A, X1: deploy a SINGLETON vm/instance block
through the real block pipeline (`expanse ctl block apply`, not a
standalone probe) and confirm the guest actually boots, is reachable at
its pinned macvtap network identity (D2) from another cluster node, and
has written to its raw, replicated disk (D1/D3) -- the runtime glue
`PHASE-06-TASKS.md` D1's own recommendation named as still unmeasured
after the two standalone probes (vm-d1-boot-probe.nix, vm-macvtap-probe.nix).

Runs after cluster-common.py, block-common.py and vol_cluster.py.
Expects KERNEL, INITRD, CMDLINE and GUEST_IP (vm-instance.nix) spliced
in ahead of this file.
"""

NAME = "vm1"
DISK_MIB = 16

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
      size: {DISK_MIB}Mi
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

form("vminst")
wait_agent_ready(n1)
wait_agent_ready(n2)
wait_agent_ready(n3)
ALL = {"n1": n1, "n2": n2, "n3": n3}

with subtest("deploy a SINGLETON vm/instance block bound to a raw volume (X1)"):
    deploy(n1, NAME, MANIFEST)
    # Generous: a fresh volume must resync, then qemu-kvm has to boot a
    # real (if minimal) guest kernel under nested KVM before this ever
    # reaches RUNNING -- the same class of budget vm-d1-boot-probe.nix's
    # own measured runs needed.
    b = wait_phase(n1, NAME, ["RUNNING"], 300)
    nodes = placement_nodes(b)
    assert len(nodes) == 1, f"vm instance placed on {nodes}, want exactly 1: {b.get('status')}"
    host_name = next(iter(nodes))
    host = ALL[host_name]

with subtest("the instance's unit is active on its placement node, running as root (D2's own privilege need)"):
    # Restart=on-failure/RestartSec=5s (nix/modules/agent.nix) already
    # recovers a transient first-attempt failure on its own -- checked
    # with a retry, not a single instant read, for the same reason every
    # other unit-readiness check in this suite does.
    host.wait_until_succeeds(
        f"systemctl is-active expanse-block-root@default-{NAME}-0.service", timeout=60
    )

with subtest("the instance places on (and only on) the volume's DRBD primary (D3/P12)"):
    # The storage controller's own colocation move (P12) converges in a
    # separate reconcile pass from the block placement that already
    # landed -- confirmed racing this check directly ("primary moves to
    # block host" logged well after the unit was already active), not
    # simultaneous with it.
    res = host.succeed("drbdadm status | head -1 | cut -d' ' -f1").strip()
    wait_for(lambda: role_of(host, res) == "Primary",
             f"{host_name} to become DRBD primary for {res}", timeout=120)

with subtest("the guest is reachable over the LAN at its pinned macvtap identity (D2, X1)"):
    others = [m for n, m in ALL.items() if n != host_name]
    # Any survivor's root netns reaching the guest directly (no VIP, no
    # proxy) is exactly D2's own measured mechanism -- not the "host
    # can't reach its own macvtap child" limitation, which only applies
    # to the HOST node's own root netns against ITS OWN macvtap child.
    others[0].wait_until_succeeds(f"ping -c1 -W2 {GUEST_IP}", timeout=180)
    print(others[0].succeed(f"ping -c2 -W2 {GUEST_IP}"))

with subtest("the guest wrote its boot marker to the raw, replicated disk (D1/D3)"):
    dev = device_of(host)
    out = host.succeed(f"od -c {dev} | head -3")
    print(out)
    assert "C   A   N   A   R   Y   -   O   K" in out, \
        f"canary marker not found on {dev}: {out!r}"

with subtest("the raw storage entry was never formatted or mounted (D3, unchanged from Phase 4)"):
    dev = device_of(host)
    fstype = host.succeed(f"blkid -o value -s TYPE {dev} 2>/dev/null || true").strip()
    assert fstype == "", f"raw disk {dev} was formatted: TYPE={fstype!r}"
    mounts = host.succeed(f"grep -F /mnt/{NAME}-disk /proc/mounts || true").strip()
    assert mounts == "", f"{host_name}: /mnt/{NAME}-disk unexpectedly mounted: {mounts!r}"


def unit_of(name):
    return f"expanse-block-root@default-{name}-0.service"


def status_text(m, name):
    return m.succeed(f"systemctl show -p StatusText --value {unit_of(name)}").strip()


with subtest("the booted guest reported ready over vsock, and that is what made it RUNNING"):
    text = status_text(host, NAME)
    print(f"{NAME} status: {text}")
    assert text.startswith("ready:"), f"{NAME} is RUNNING but its unit status is {text!r}"

STUCK = "vm2"
WAITING = "not ready: guest started, waiting for multi-user.target"
with subtest("a guest that starts but never reaches multi-user.target is never RUNNING"):
    # basic.target sends READY=1 (as emergency mode does) but never multi-user.target. Emergency mode
    # itself is no good here: its shell reads EOF from the console and the guest carries on booting.
    deploy(n1, STUCK, MANIFEST.replace(f"name: {NAME}", f"name: {STUCK}")
           .replace(f"/mnt/{NAME}-disk", f"/mnt/{STUCK}-disk")
           .replace(f'bootCmdline: "{CMDLINE}"', f'bootCmdline: "{CMDLINE} systemd.unit=basic.target"'))

    def stuck_node():
        nodes = placement_nodes(get_json(n1, STUCK) or {})
        return next(iter(nodes)) if len(nodes) == 1 else ""

    wait_for(lambda: stuck_node() != "", f"{STUCK} placed", timeout=300)
    stuck_host = ALL[stuck_node()]
    stuck_host.wait_until_succeeds(f"systemctl is-active {unit_of(STUCK)}", timeout=300)
    wait_for(lambda: status_text(stuck_host, STUCK) == WAITING, f"{STUCK} to report {WAITING!r}", timeout=300)
    time.sleep(30)  # many controller passes, any one of which would promote a ready replica
    text = status_text(stuck_host, STUCK)
    phase = ((get_json(n1, STUCK) or {}).get("status") or {}).get("phase", "")
    print(f"{STUCK}: phase {phase}, status {text!r}")
    assert text == WAITING, f"{STUCK} status moved on to {text!r}"
    assert phase != "RUNNING", f"{STUCK} is RUNNING although its guest never reached multi-user.target"

print("VM-INSTANCE DONE")
