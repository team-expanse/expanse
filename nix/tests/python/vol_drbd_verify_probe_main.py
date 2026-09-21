"""vol-drbd-verify-probe testScript body (Phase 1 C4b).

Records how DRBD 9 online verify and invalidate behave on three real nodes: what the status shows
while a verify runs and after it, what a mismatch looks like, what repairs it, and whether
invalidate is refused on a primary. Observations only.
"""

import json
import time

MACHINES = [n1, n2, n3]
SIZE_MIB = 400


def status(m):
    out = m.succeed("drbdsetup status r0 --json --statistics")
    return json.loads(out)[0]


def snap(m):
    st = status(m)
    return [(p["name"], p["connection-state"], [(v["replication-state"], v["peer-disk-state"], v["out-of-sync"]) for v in p["peer_devices"]]) for p in st["connections"]]


def probe(label, m, cmd):
    rc, out = m.execute(f"{cmd} 2>&1")
    print(f"PROBE [{label}] {m.name}: `{cmd}` rc={rc}\n{out.strip()}\n---")


def trace(label, m, secs):
    """Poll the status until it stops changing for a while; print each change."""
    began, last, quiet = time.time(), None, 0
    while time.time() - began < secs:
        cur = snap(m)
        if cur != last:
            print(f"TRACE [{label}] +{time.time() - began:5.1f}s {cur}")
            last, quiet = cur, 0
        else:
            quiet += 1
        time.sleep(0.1)
    print(f"TRACE [{label}] end: {last}")


def dmesg(m, pattern):
    print(f"DMESG {m.name} [{pattern}]:\n{m.execute(f'dmesg | grep -i \"{pattern}\" | tail -8')[1]}")


start_all()
for m in MACHINES:
    m.wait_for_unit("multi-user.target")
    m.succeed("modprobe drbd")
    m.succeed("drbdadm create-md --force --max-peers=7 r0")
    m.succeed("drbdadm up r0")
n1.succeed("drbdadm primary --force r0")
n1.wait_until_succeeds("test $(drbdadm status r0 | grep -c UpToDate) -eq 3", timeout=120)
n1.succeed(f"dd if=/dev/urandom of=/dev/drbd0 bs=1M count={SIZE_MIB - 8} oflag=direct")
n1.succeed("sync")

print("=== 1. verify on a clean, connected, 3-node resource (from the primary)")
began = time.time()
probe("verify", n1, "drbdadm verify r0")
trace("clean verify", n1, 25)
print(f"clean verify wall time to quiet: {time.time() - began:.1f}s")
dmesg(n1, "verify")

print("=== 2. corrupt n2's backing device under DRBD, then verify")
n2.succeed("drbdadm down r0")
n2.succeed("dd if=/dev/urandom of=/dev/vdb bs=1M seek=100 count=2 oflag=direct conv=notrunc")
n2.succeed("drbdadm up r0")
n1.wait_until_succeeds("test $(drbdadm status r0 | grep -c UpToDate) -eq 3", timeout=60)
trace("after corrupt+up", n1, 5)
probe("verify after corruption", n1, "drbdadm verify r0")
trace("dirty verify", n1, 25)
dmesg(n1, "verify")
dmesg(n1, "out of sync")
probe("status after dirty verify", n1, "drbdadm status r0")

print("=== 3. does a repair happen by itself, or need disconnect/connect?")
trace("idle after verify", n1, 8)
probe("disconnect", n1, "drbdadm disconnect r0")
probe("connect", n1, "drbdadm connect r0")
trace("after reconnect", n1, 25)
probe("status", n1, "drbdadm status r0")

print("=== 4. verify again: clean?")
probe("verify", n1, "drbdadm verify r0")
trace("second verify", n1, 25)
dmesg(n1, "verify")

print("=== 5. verify started on a secondary")
probe("verify from secondary", n3, "drbdadm verify r0")
trace("verify from n3", n3, 25)

print("=== 6. invalidate")
probe("invalidate on the primary", n1, "drbdadm invalidate r0")
trace("after refused invalidate", n1, 3)
probe("invalidate on a secondary", n3, "drbdadm invalidate r0")
trace("invalidate n3, seen from n3", n3, 30)
trace("invalidate n3, seen from n1", n1, 3)
n1.wait_until_succeeds("test $(drbdadm status r0 | grep -c UpToDate) -eq 3", timeout=120)
probe("status", n1, "drbdadm status r0")
sums = {m.name: m.succeed(f"dd if=/dev/vdb bs=1M count={SIZE_MIB - 8} 2>/dev/null | md5sum").split()[0] for m in MACHINES}
print(f"REPLICA SUMS {sums}")
print("VERIFY PROBE DONE")
