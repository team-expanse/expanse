"""Helpers shared by the volume VM tests (vol-runtime, vol-primary).

Prepended to each test's script, so the test defines NAME, DB, ADDRS and by_name
first: every function here looks them up when it is called.
"""

import json
import re
import time


def volctl(m, args):
    return m.succeed(f"volctl {args}")


def setup_node(m):
    m.wait_for_unit("multi-user.target")
    m.succeed("mkdir -p /etc/drbd.d /var/lib/drbd && modprobe drbd")
    m.succeed("vgcreate vg0 /dev/vdb && lvcreate --yes --type thin-pool -L 512M -n pool vg0")


def desired(host, size):
    out = volctl(n1, f"desired -db {DB} -name {NAME} -self {host} -size {size} -addrs {ADDRS}")
    return out.strip()


def reconcile(host, size):
    """One pass on host; returns (actions, forgot)."""
    m = by_name[host]
    m.succeed(f"cat > /tmp/d.json <<'EOF'\n{desired(host, size)}\nEOF")
    res = json.loads(volctl(m, "reconcile -desired /tmp/d.json"))
    return res.get("Actions") or [], res.get("Forgot") or []


def status(m):
    return m.succeed(f"drbdadm status {NAME}")


def wait_until(m, predicate, what, timeout=180):
    deadline = time.time() + timeout
    text = ""
    while time.time() < deadline:
        text = m.execute(f"drbdadm status {NAME} 2>&1")[1]
        if predicate(text):
            return
        time.sleep(1)
    raise Exception(f"timed out waiting for {what} on {m.name}; last status:\n{text}")


def connected_to(peers):
    return lambda text: all(re.search(rf"^\s+{p} role:", text, re.M) for p in peers)


def all_uptodate(peers):
    def check(text):
        return "disk:UpToDate" in text.split("\n")[1] and text.count("peer-disk:UpToDate") == len(peers)
    return check


def device_bytes(m):
    # sysfs, because a Secondary device cannot be opened
    return int(m.succeed("cat /sys/block/drbd0/size").strip()) * 512


def head_sha(m, count_mib=32):
    return m.succeed(f"dd if=/dev/drbd0 bs=1M count={count_mib} iflag=direct 2>/dev/null | sha256sum").split()[0]


def observe(m, ids):
    """volctl observe on m: {host: replica}, plus 'quorum'. ids maps host -> node-id."""
    members = ",".join(f"{h}={i}" for h, i in ids.items())
    out = json.loads(volctl(m, f"observe -name {NAME} -self {m.name} -members {members}"))
    view = {r["NodeID"]: r for r in out["Replicas"]}
    view["quorum"] = out["Quorum"]
    return view


def roles(view):
    """{host: (Role, Healthy)} of an observe() view."""
    return {h: (r["Role"], r["Healthy"]) for h, r in view.items() if h != "quorum"}
