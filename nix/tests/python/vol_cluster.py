"""DRBD helpers shared by the three-node vol-* VM tests (vol-agent, vol-durability).

Runs after cluster-common.py, which provides re, time and the n1..n3 machines.
"""

NODES = [n1, n2, n3]


def drbd_status(m, res):
    return m.execute(f"drbdadm status {res} 2>&1")[1]


def role_of(m, res):
    found = re.search(r"role:(\w+)", drbd_status(m, res).split("\n")[0])
    return found.group(1) if found else ""


def primaries(res, nodes=NODES):
    return [m for m in nodes if role_of(m, res) == "Primary"]


def fully_replicated(m, res):
    text = drbd_status(m, res)
    lines = text.split("\n")
    return len(lines) > 1 and "disk:UpToDate" in lines[1] and text.count("peer-disk:UpToDate") == 2


def wait_for(predicate, what, timeout=240):
    deadline = time.time() + timeout
    while time.time() < deadline:
        if predicate():
            return
        time.sleep(2)
    raise Exception(f"timed out waiting for {what}")


def device_of(m):
    return "/dev/" + m.succeed("ls /dev | grep -E '^drbd[0-9]+$' | head -1").strip()
