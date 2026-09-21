"""vol-agent: the wired agent takes `ctl volume create` to a replicated DRBD volume,
fails it over when the primary's agent stops, and tears it down on delete.

Runs after cluster-common.py, which provides form(), re, time and the n1..n3 machines.
"""

NODES = [n1, n2, n3]
SIZE_MIB = 16


def drbd_status(m, res):
    return m.execute(f"drbdadm status {res} 2>&1")[1]


def role_of(m, res):
    found = re.search(r"role:(\w+)", drbd_status(m, res).split("\n")[0])
    return found.group(1) if found else ""


def primaries(res):
    return [m for m in NODES if role_of(m, res) == "Primary"]


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


def sha_of_device(m):
    dev = device_of(m)
    return m.succeed(f"dd if={dev} bs=1M count={SIZE_MIB} iflag=direct 2>/dev/null | sha256sum | cut -d' ' -f1").strip()


form("voldrbd")

with subtest("create places one replica on every node"):
    out = n1.succeed(f"expanse ctl volume create testvol --size {SIZE_MIB * 2}Mi --replication 3")
    assert "create requested" in out, out
    for m in NODES:
        m.wait_until_succeeds("drbdadm status | grep -q '^vol-'", timeout=180)
    res = n1.succeed("drbdadm status | head -1 | cut -d' ' -f1").strip()
    for m in NODES:
        m.succeed(f"test -e /etc/drbd.d/{res}.res")
        m.succeed(f"lvs --noheadings -o lv_name vg0 | grep -qx '\\s*{res}\\s*'")

with subtest("all replicas reach UpToDate with exactly one Primary"):
    wait_for(lambda: all(fully_replicated(m, res) for m in NODES), "every replica UpToDate")
    wait_for(lambda: len(primaries(res)) == 1, "one primary")

with subtest("the controller reports the volume Healthy"):
    n1.wait_until_succeeds("expanse ctl volume list | grep -i healthy", timeout=120)

with subtest("data written on the primary reads back intact"):
    primary = primaries(res)[0]
    primary.succeed(f"dd if=/dev/urandom of=/tmp/pad bs=1M count={SIZE_MIB}")
    ref = primary.succeed("sha256sum /tmp/pad | cut -d' ' -f1").strip()
    primary.succeed(f"dd if=/tmp/pad of={device_of(primary)} bs=1M oflag=direct conv=fsync")
    assert sha_of_device(primary) == ref, "device does not hold what was written"

with subtest("stopping the primary's agent moves the primary and keeps the data"):
    old = primaries(res)[0]
    old.succeed("systemctl stop expansed.service")
    survivors = [m for m in NODES if m is not old]
    wait_for(lambda: len([m for m in survivors if role_of(m, res) == "Primary"]) == 1, "a new primary", 300)
    new = [m for m in survivors if role_of(m, res) == "Primary"][0]
    assert role_of(old, res) != "Primary", "the stopped node is still primary"
    assert sha_of_device(new) == ref, "data lost across the failover"

with subtest("the returned node rejoins as a Secondary and stays so"):
    old.succeed("systemctl start expansed.service")
    old.wait_for_unit("expansed.service")
    wait_for(lambda: all(fully_replicated(m, res) for m in NODES), "old primary resynced")
    time.sleep(20)
    assert len(primaries(res)) == 1, f"primaries: {[m.name for m in primaries(res)]}"

with subtest("delete removes the replica everywhere and frees the identity"):
    n1.succeed("expanse ctl volume delete testvol")
    for m in NODES:
        m.wait_until_succeeds(f"! drbdadm status {res} >/dev/null 2>&1", timeout=180)
        m.wait_until_succeeds(f"! lvs --noheadings -o lv_name vg0 | grep -q {res}", timeout=60)
        m.succeed(f"! test -e /etc/drbd.d/{res}.res")
    n1.wait_until_succeeds("expanse ctl volume list | grep -q 'no volumes'", timeout=120)
