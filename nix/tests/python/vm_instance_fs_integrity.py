"""PHASE-06-TASKS.md Stream C (X4): a vm/instance block's GUEST filesystem
survives a hard node kill under a sustained write load actually in flight
at the moment of the kill -- zero corruption, every acknowledged write
intact. Strictly stronger than Stream B's X2 (raw bytes survive): here
the guest's own ext4 must fsck clean and account for every file its own
write loop had fsynced before the kill.

Runs after cluster-common.py, block-common.py and vol_cluster.py.
Expects KERNEL, INITRD, CMDLINE, GUEST_IP and UNIT
(vm-instance-fs-integrity.nix) spliced in ahead of this file.
"""

NAME = "vm1"
DISK_MIB = 64

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


def console_text(m):
    """This node's own guest console output, captured by the block unit's
    stdout (runVM's '-serial stdio') and forwarded to its journal."""
    return m.succeed(f"journalctl -u {UNIT} --no-pager -o cat 2>/dev/null || true")


def last_write_index(text):
    found = re.findall(r"STATUS write index=(\d+)", text)
    return int(found[-1]) if found else 0


def last_verify(text):
    """Most recent post-mount reconciliation the guest reported, or None
    if it hasn't run one yet (still booting, or genuinely first boot)."""
    found = re.findall(r"STATUS verify last=(\d+) ok=(\d+) bad=(\d+) fsck_rc=(-?\d+)", text)
    if not found:
        return None
    last, ok, bad, fsck_rc = found[-1]
    return {"last": int(last), "ok": int(ok), "bad": int(bad), "fsck_rc": int(fsck_rc)}


form("vmfsinteg")
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

with subtest("the instance places on the volume's DRBD primary and the guest boots"):
    res = host.succeed("drbdadm status | head -1 | cut -d' ' -f1").strip()
    wait_for(lambda: role_of(host, res) == "Primary",
             f"{holder} to become DRBD primary for {res}", timeout=120)
    others_before = [n for n in ALL if n != holder]
    ALL[others_before[0]].wait_until_succeeds(f"ping -c1 -W2 {GUEST_IP}", timeout=180)

with subtest("the guest formats ext4 and its write load is actively running"):
    wait_for(lambda: last_write_index(console_text(host)) >= 20,
             "guest write-load to reach index >= 20", timeout=180)
    idx_before = last_write_index(console_text(host))
    print(f"pre-kill: write-load reached index={idx_before}")

with subtest("kill the holder's whole VM mid-write"):
    t0 = time.time()
    ALL[holder].crash()

with subtest("the block and its volume primary re-converge on one survivor"):
    survivor = next(iter(m for n, m in ALL.items() if n != holder))
    deadline = time.time() + 240
    new_holder = None
    last_seen = {}
    while time.time() < deadline:
        b = get_json(survivor, NAME)
        cur_nodes = placement_nodes(b) if b else set()
        if len(cur_nodes) == 1 and holder not in cur_nodes:
            candidate = next(iter(cur_nodes))
            primary_role = role_of(ALL[candidate], res)
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
    print(f"instance and its volume primary agree on {new_holder} after {time.time() - t0:.1f}s")

with subtest("the instance's unit is active as root on the new holder"):
    new_host.wait_until_succeeds(
        f"systemctl is-active expanse-block-root@default-{NAME}-0.service", timeout=60
    )

with subtest("the guest fscks clean and every acknowledged write survived intact (X4)"):
    # Same boot-time budget X1/X2 already needed under nested KVM -- a
    # fresh cold boot, ext4 journal recovery, e2fsck -f and the file
    # reconciliation loop all happen before diskinit prints its verdict.
    wait_for(lambda: last_verify(console_text(new_host)) is not None,
             f"{new_holder}'s guest to fsck, mount and verify its prior writes", timeout=300)
    result = last_verify(console_text(new_host))
    print(f"post-failover verify: {result} (pre-kill index was {idx_before})")
    assert result["fsck_rc"] < 4, \
        f"e2fsck reported uncorrectable filesystem errors: rc={result['fsck_rc']}"
    assert result["last"] >= 15, \
        f"suspiciously few acknowledged writes survived: {result}"
    assert result["bad"] == 0, \
        f"{result['bad']} of {result['last']} acknowledged writes were missing or corrupt: {result}"
    assert result["ok"] == result["last"], f"ok/last mismatch: {result}"

with subtest("the guest is still reachable at its unchanged macvtap identity"):
    third = [n for n in ALL if n not in (holder, new_holder)][0]
    ALL[third].wait_until_succeeds(f"ping -c1 -W2 {GUEST_IP}", timeout=180)
    print(ALL[third].succeed(f"ping -c2 -W2 {GUEST_IP}"))

print("VM-INSTANCE-FS-INTEGRITY DONE")
