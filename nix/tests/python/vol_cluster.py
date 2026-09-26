"""DRBD helpers shared by the three-node vol-* VM tests.

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


def volume_row(m, name):
    """The controller's row for the named volume as m sees it, or None."""
    for line in m.execute("expanse ctl volume list 2>&1")[1].splitlines():
        cols = line.split()
        if len(cols) >= 6 and cols[1] == name and cols[0] != "-":  # "-" marks a pending request
            return {"id": cols[0], "state": cols[3].lower().removeprefix("volume_state_"), "nodes": sorted(cols[5].split(","))}
    return None


def connection_of(m, res, peer):
    """m's DRBD connection state to peer: DRBD prints none while it is Connected."""
    found = re.search(rf"^\s+{peer.name} connection:(\w+)", drbd_status(m, res), re.M)
    return found.group(1) if found else "Connected"


def device_of(m):
    return "/dev/" + m.succeed("ls /dev | grep -E '^drbd[0-9]+$' | head -1").strip()


def checksum(m, dev, mib):
    """sha256 of the first mib MiB of dev, read past the page cache."""
    return m.succeed(f"dd if={dev} bs=1M count={mib} iflag=direct 2>/dev/null | sha256sum | cut -d' ' -f1").strip()


def fill_paced(m, dev, mib, chunk_mib=64, pause_s=0.5):
    """Random data in bursts with pauses: one long dd saturates the shared test link and trips a raft election."""
    for seek in range(0, mib, chunk_mib):
        m.succeed(f"dd if=/dev/urandom of={dev} bs=1M seek={seek} count={chunk_mib} oflag=direct conv=fsync,notrunc")
        time.sleep(pause_s)
