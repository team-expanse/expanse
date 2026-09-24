"""PHASE-06-TASKS.md Stream B (X2, the phase's decider; X3): a SINGLETON
vm/instance block survives losing the node running it. The holder is
hard-killed mid-run; the raw disk's DRBD primary must promote on a
survivor, the block scheduler must reschedule the instance there
automatically, and the guest must cold-boot with its own filesystem
intact -- proved by a pre-kill marker surviving and a post-restart
marker proving the guest is genuinely alive again against the SAME
disk, not a fresh one (X2). A third, uninvolved node must then reach
the guest at its unchanged, pinned macvtap identity with no
client-side reconfiguration at all (X3).

Runs after cluster-common.py, block-common.py and vol_cluster.py.
Expects KERNEL, INITRD, CMDLINE and GUEST_IP (vm-instance-failover.nix)
spliced in ahead of this file.
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

ALL = {"n1": n1, "n2": n2, "n3": n3}


def read_sector(m, dev, sector):
    """One aligned 512B sector, bypassing the host's page cache -- a qemu
    guest write issued O_DIRECT is otherwise invisible to a plain buffered
    dd read here (vol_cluster.checksum's own reason for iflag=direct)."""
    return m.succeed(f"dd if={dev} bs=512 count=1 skip={sector} iflag=direct 2>/dev/null | tr -d '\\0'")


def boot_marker(m, dev):
    """The 'BOOT-<n>' counter at its fixed offset, or '' before first write."""
    found = re.search(r"BOOT-\d+", read_sector(m, dev, 1))
    return found.group(0) if found else ""


def origin_marker(m, dev):
    return read_sector(m, dev, 0)[:9]


form("vminstfo")
wait_agent_ready(n1)
wait_agent_ready(n2)
wait_agent_ready(n3)

with subtest("deploy a SINGLETON vm/instance block and reach RUNNING"):
    deploy(n1, NAME, MANIFEST)
    b = wait_phase(n1, NAME, ["RUNNING"], 300)
    nodes = placement_nodes(b)
    assert len(nodes) == 1, f"vm instance placed on {nodes}, want exactly 1: {b.get('status')}"
    holder = next(iter(nodes))
    host = ALL[holder]

with subtest("the instance places on the volume's DRBD primary and the guest boots (X1 baseline)"):
    res = host.succeed("drbdadm status | head -1 | cut -d' ' -f1").strip()
    wait_for(lambda: role_of(host, res) == "Primary",
             f"{holder} to become DRBD primary for {res}", timeout=120)
    others_before = [n for n in ALL if n != holder]
    ALL[others_before[0]].wait_until_succeeds(f"ping -c1 -W2 {GUEST_IP}", timeout=180)

with subtest("the guest's own boot markers are present before the kill"):
    dev = device_of(host)
    assert origin_marker(host, dev) == "CANARY-OK", \
        f"pre-kill origin marker missing on {dev}: {origin_marker(host, dev)!r}"
    marker_before = boot_marker(host, dev)
    assert marker_before.startswith("BOOT-"), f"pre-kill boot marker missing: {marker_before!r}"
    print(f"pre-kill markers: origin=CANARY-OK boot={marker_before!r}")

with subtest("kill the holder's whole VM"):
    t0 = time.time()
    ALL[holder].crash()

with subtest("the block and its volume primary re-converge on one survivor (X2)"):
    survivors = {n: m for n, m in ALL.items() if n != holder}
    survivor = next(iter(survivors.values()))
    deadline = time.time() + 240
    new_holder = None
    last_seen = {}
    while time.time() < deadline:
        b = get_json(survivor, NAME)
        cur_nodes = placement_nodes(b) if b else set()
        if len(cur_nodes) == 1 and holder not in cur_nodes:
            candidate = next(iter(cur_nodes))
            candidate_m = ALL[candidate]
            primary_role = role_of(candidate_m, res)
            last_seen = {"placement": candidate, "primary_role": primary_role}
            if primary_role == "Primary":
                new_holder = candidate
                break
        else:
            last_seen = {"placement": sorted(cur_nodes)}
        time.sleep(2)
    assert new_holder, \
        f"instance and its volume primary never agreed on one survivor within 240s: {last_seen}"
    new_host = ALL[new_holder]
    reconverge_s = time.time() - t0
    print(f"instance and its volume primary agree on {new_holder} after {reconverge_s:.1f}s")

with subtest("the instance's unit is active as root on the new holder"):
    new_host.wait_until_succeeds(
        f"systemctl is-active expanse-block-root@default-{NAME}-0.service", timeout=60
    )

with subtest("the guest is reachable again at its unchanged macvtap identity, no client reconfig (X3)"):
    third = [n for n in ALL if n not in (holder, new_holder)][0]
    ALL[third].wait_until_succeeds(f"ping -c1 -W2 {GUEST_IP}", timeout=180)
    print(ALL[third].succeed(f"ping -c2 -W2 {GUEST_IP}"))

with subtest("the guest cold-booted with its filesystem intact: same disk, genuinely alive again (X2)"):
    new_dev = device_of(new_host)
    # The origin marker is a DRBD-replicated byte, present the instant the
    # new primary is UpToDate -- no guest boot required to see it. The
    # boot counter needs the guest to actually finish booting and run its
    # canary service, which (like X1's own first boot) can take minutes
    # under nested KVM; reachability (X3, above) only proves the network
    # stack is up, not that multi-user.target -- and this oneshot -- ran.
    wait_for(lambda: origin_marker(new_host, new_dev) == "CANARY-OK",
             f"origin marker to survive on {new_holder}'s {new_dev}", timeout=60)
    wait_for(lambda: boot_marker(new_host, new_dev) != marker_before,
             f"boot marker to advance past {marker_before!r} on {new_holder}'s {new_dev}", timeout=300)
    marker_after = boot_marker(new_host, new_dev)
    print(f"post-failover markers: origin=CANARY-OK boot={marker_after!r} (was {marker_before!r})")

print("VM-INSTANCE-FAILOVER DONE")
