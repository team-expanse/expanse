"""PHASE-06-TASKS.md Stream D: the phase-closing vertical slice -- X3
(network identity survives) and X4 (guest filesystem integrity) proven
TOGETHER against the SAME kill, in one run, rather than each stream's
own narrower test killing separately.

Unlike Stream B's vm_instance_failover.py and Stream C's
vm_instance_fs_integrity.py, which each explicitly wait for the holder
to become the volume's DRBD primary before doing anything else, this
test stays black-box: it deploys, waits only until the guest's own
write load is confirmed flowing (which cannot happen at all unless the
disk is already mounted, i.e. unless DRBD primary + guest boot already
happened), and kills whichever node placement says currently holds the
instance -- "kill mid-deploy, not after an assumed internal
convergence" (PHASE-04-TASKS.md's own vertical-slice framing, reused
here). X3 is also measured quantitatively this time (elapsed time from
the kill to the first successful post-kill ping), not just checked as
an eventual boolean.

Runs after cluster-common.py, block-common.py and vol_cluster.py.
Expects KERNEL, INITRD, CMDLINE, GUEST_IP and UNIT
(vm-instance-vertical-slice.nix) spliced in ahead of this file.
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


def can_ping(m, ip):
    status, _ = m.execute(f"ping -c1 -W1 {ip}")
    return status == 0


form("vmvslice")
wait_agent_ready(n1)
wait_agent_ready(n2)
wait_agent_ready(n3)

with subtest("deploy a SINGLETON vm/instance block and reach RUNNING"):
    # No internal DRBD-primary-role wait here on purpose (unlike Stream
    # B/C) -- placement alone is the only pre-kill signal this test
    # relies on, mirroring iscsi-vertical-slice.nix's own black-box stance.
    deploy(n1, NAME, MANIFEST)
    b = wait_phase(n1, NAME, ["RUNNING"], 300)
    nodes = placement_nodes(b)
    assert len(nodes) == 1, f"vm instance placed on {nodes}, want exactly 1: {b.get('status')}"
    holder = next(iter(nodes))
    host = ALL[holder]
    uninvolved = [n for n in ALL if n != holder][0]

with subtest("the guest's write load is confirmed actively flowing"):
    # This can only succeed once the disk is mounted, which can only
    # happen once DRBD primary + guest boot already happened -- proof
    # by consequence, not by an explicit internal-state check.
    wait_for(lambda: last_write_index(console_text(host)) >= 20,
             "guest write-load to reach index >= 20", timeout=240)
    idx_before = last_write_index(console_text(host))
    ALL[uninvolved].wait_until_succeeds(f"ping -c1 -W2 {GUEST_IP}", timeout=60)
    print(f"pre-kill: write-load reached index={idx_before}, guest reachable from {uninvolved}")

with subtest("kill whichever node currently holds the instance, the instant I/O is flowing"):
    t0 = time.time()
    ALL[holder].crash()

with subtest("the block and its volume primary re-converge on one survivor"):
    # Host-level convergence (placement + DRBD primary) and guest-level
    # boot are on genuinely different timescales -- settling this first,
    # as its own loop, rather than interleaved with ping attempts that
    # can't succeed yet, is what makes X3's own timing loop below
    # meaningful instead of racing a guest that hasn't booted at all.
    survivor = next(iter(m for n, m in ALL.items() if n != holder))
    deadline = time.time() + 240
    new_holder = None
    last_seen = {}
    while time.time() < deadline:
        b = get_json(survivor, NAME)
        cur_nodes = placement_nodes(b) if b else set()
        if len(cur_nodes) == 1 and holder not in cur_nodes:
            candidate = next(iter(cur_nodes))
            res = ALL[candidate].succeed("drbdadm status | head -1 | cut -d' ' -f1").strip()
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
    reconverge_s = time.time() - t0
    print(f"instance and its volume primary agree on {new_holder} after {reconverge_s:.1f}s")

with subtest("the instance's unit is active as root on the new holder"):
    new_host.wait_until_succeeds(
        f"systemctl is-active expanse-block-root@default-{NAME}-0.service", timeout=60
    )

with subtest("the guest is reachable at its unchanged macvtap identity, resume time measured (X3)"):
    # The genuinely uninvolved node -- whichever survivor did NOT win
    # placement -- regardless of which one "uninvolved" guessed in
    # advance, so this never risks hitting the known
    # host-cannot-reach-its-own-macvtap-guest limitation (A34).
    third = [n for n in ALL if n not in (holder, new_holder)][0]
    deadline = time.time() + 240
    resumed_at = None
    while time.time() < deadline:
        if can_ping(ALL[third], GUEST_IP):
            resumed_at = time.time() - t0
            break
        time.sleep(1)
    assert resumed_at is not None, f"{third} never reached the guest within 240s of the kill"
    print(f"X3: guest reachable from {third} {resumed_at:.1f}s after kill")

with subtest("the guest fscks clean and every acknowledged write survived intact (X4)"):
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

print("VM-INSTANCE-VERTICAL-SLICE DONE")
