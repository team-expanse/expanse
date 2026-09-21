"""vol-primary testScript body (Phase 1 B5).

Drives the promotion gate (test/volctl hold) on real DRBD across three VMs. A file
stands in for the Raft lease (rm revokes it); the real lease is covered by unit tests.
Covers the initial forced promotion, demotion on lease loss with a mounted
filesystem, failover, a second primary being refused, quorum loss, shutdown and
what a killed agent leaves behind. Also checks what volctl observe (B7) reports
from each node through a slow resync, a failover and quorum loss.
"""

NAME = "vol-a1"
NODES = {"n1": "192.168.1.1", "n2": "192.168.1.2", "n3": "192.168.1.3"}
ADDRS = ",".join(f"{h}={a}" for h, a in NODES.items())
DB = "/tmp/alloc.db"
SIZE = 64 * 1024 * 1024
MNT = "/mnt/vol"
LEASE = "/run/lease-vol"
by_name = {"n1": n1, "n2": n2, "n3": n3}
PEERS = {h: [p for p in NODES if p != h] for h in NODES}
IDS = {"n1": 0, "n2": 1, "n3": 2}


def role(m):
    return m.succeed(f"drbdadm role {NAME}").strip().split("/")[0]


def wait_role(m, want, timeout=60):
    deadline = time.time() + timeout
    while time.time() < deadline:
        if role(m) == want:
            return
        time.sleep(0.2)
    raise Exception(f"{m.name} did not become {want}; is {role(m)}\n{hold_log(m)}")


def has_quorum(m):
    return json.loads(m.succeed(f"drbdsetup status {NAME} --json"))[0]["devices"][0]["quorum"]


def wait_quorum(m, want, timeout=90):
    deadline = time.time() + timeout
    while time.time() < deadline:
        if has_quorum(m) == want:
            return
        time.sleep(0.5)
    raise Exception(f"{m.name} quorum never became {want}\n{status(m)}")


def start_hold(m, initial=False):
    m.succeed(f"touch {LEASE}")
    flag = "-initial" if initial else ""
    m.succeed(
        f"systemd-run --unit hold-{NAME} --setenv=PATH=/run/current-system/sw/bin "
        f"--property=StandardOutput=file:/tmp/hold.out "
        f"--property=StandardError=file:/tmp/hold.err "
        f"volctl hold -name {NAME} -lease-file {LEASE} -mount-dir {MNT} {flag}"
    )


def hold_log(m):
    return m.execute("cat /tmp/hold.out /tmp/hold.err 2>&1")[1]


def hold_active(m):
    return m.execute(f"systemctl is-active hold-{NAME}")[1].strip() == "active"


def revoke(m):
    """Revoke the lease and wait for the promoter to finish stepping down."""
    m.succeed(f"rm -f {LEASE}")
    deadline = time.time() + 30
    while hold_active(m) and time.time() < deadline:
        time.sleep(0.2)
    assert not hold_active(m), f"{m.name}: promoter did not exit\n{hold_log(m)}"
    m.execute(f"systemctl reset-failed hold-{NAME}")


def partition(m):
    for p in PEERS[m.name]:
        ip = NODES[p]
        m.succeed(f"iptables -I INPUT -s {ip} -j DROP && iptables -I OUTPUT -d {ip} -j DROP")


def heal(m):
    m.succeed("iptables -F")


def mounted(m):
    return m.execute(f"mountpoint -q {MNT}")[0] == 0


start_all()
for m in by_name.values():
    setup_node(m)
    m.succeed(f"mkdir -p {MNT}")

with subtest("three replicas defined and connected"):
    volctl(n1, f"alloc -db {DB} -name {NAME} -hosts n1,n2,n3")
    for h in NODES:
        reconcile(h, SIZE)
    for h in NODES:
        wait_until(by_name[h], connected_to(PEERS[h]), "peers connected")

with subtest("a fresh volume is not forced while a replica is unreachable"):
    n1.succeed(f"drbdsetup disconnect {NAME} 2")  # n3's node-id
    start_hold(n1, initial=True)
    time.sleep(5)
    assert role(n1) == "Secondary", "forced a promotion with a replica missing"
    assert "primary" not in n1.succeed("cat /tmp/hold.out"), hold_log(n1)
    log = hold_log(n1)
    assert "not possible yet" in log and "not found" not in log, f"DRBD did not refuse it:\n{log}"
    print("refused plain promotion of a fresh volume:", log.strip().splitlines()[-1])
    n1.succeed(f"drbdsetup connect {NAME} 2")

with subtest("initial promotion is forced once every replica is connected and empty"):
    wait_role(n1, "Primary")
    for h in NODES:
        wait_until(by_name[h], connected_to(PEERS[h]), "reconnected")
    n1.succeed(f"mkfs.ext4 -q /dev/drbd0 && mount /dev/drbd0 {MNT}")
    n1.succeed(f"dd if=/dev/urandom of={MNT}/data bs=1M count=8 2>/dev/null && sync")
    reference = n1.succeed(f"sha256sum {MNT}/data").split()[0]
    for h in NODES:
        wait_until(by_name[h], all_uptodate(PEERS[h]), "UpToDate", timeout=240)

with subtest("observe: roles, then a slow resync seen from source, target and bystander"):
    assert roles(observe(n1, IDS)) == {
        "n1": ("Primary", True), "n2": ("Secondary", True), "n3": ("Secondary", True),
    }, observe(n1, IDS)
    # Throttle to ~100 KiB/s so an 8 MiB catch-up stays in flight for over a minute.
    slow = f"drbdsetup peer-device-options {NAME} {{peer}} 0 --c-plan-ahead=0 --resync-rate=100"
    n1.succeed(f"drbdsetup disconnect {NAME} 2")
    n3.succeed(f"drbdsetup disconnect {NAME} 0")
    n1.succeed(f"dd if=/dev/urandom of={MNT}/behind bs=1M count=8 2>/dev/null && sync")
    n1.succeed(slow.format(peer=2))
    n3.succeed(slow.format(peer=0))
    n1.succeed(f"drbdsetup connect {NAME} 2")
    n3.succeed(f"drbdsetup connect {NAME} 0")
    wait_until(n1, lambda t: "SyncSource" in t, "n3 resyncing from n1", timeout=60)

    src, tgt, bystander = observe(n1, IDS), observe(n3, IDS), observe(n2, IDS)
    print("source view:", src["n3"], "target view:", tgt["n3"], "bystander view:", bystander["n3"])
    assert (src["n3"]["Role"], src["n3"]["Healthy"]) == ("Resyncing", False), src
    assert 0 < src["n3"]["SyncPercent"] < 100 and src["n3"]["OutOfSyncKiB"] > 0, src
    assert (tgt["n3"]["Role"], tgt["n3"]["Healthy"]) == ("Resyncing", False), tgt
    assert (tgt["n1"]["Role"], tgt["n1"]["Healthy"]) == ("Primary", True), tgt
    assert (bystander["n3"]["Role"], bystander["n3"]["Healthy"]) == ("Stale", False), bystander
    assert (src["n2"]["Role"], src["n2"]["Healthy"]) == ("Secondary", True), src

    n1.succeed(f"drbdadm adjust {NAME}")
    n3.succeed(f"drbdadm adjust {NAME}")
    for h in NODES:
        wait_until(by_name[h], all_uptodate(PEERS[h]), "UpToDate", timeout=240)
    assert all(v == ("Primary" if h == "n1" else "Secondary", True)
               for h, v in roles(observe(n3, IDS)).items()), observe(n3, IDS)
    n1.succeed(f"rm {MNT}/behind")

with subtest("lease revoked while mounted: unmounted, Secondary, within seconds"):
    started = time.time()
    n1.succeed(f"rm -f {LEASE}")
    wait_role(n1, "Secondary", timeout=15)
    took = time.time() - started
    print(f"lease revoked to Secondary: {took:.1f}s")
    assert took < 10, f"demotion took {took:.1f}s"
    assert not mounted(n1), "the device stayed attached on a non-primary"
    revoke(n1)
    assert "stepped down" in hold_log(n1)

with subtest("without the consumer released, DRBD refuses to demote a mounted device"):
    n1.succeed(f"drbdadm primary {NAME} && mount /dev/drbd0 {MNT}")
    rc, out = n1.execute(f"drbdadm secondary {NAME} 2>&1")
    print("secondary while mounted:", rc, out.strip())
    assert rc != 0 and role(n1) == "Primary", "a mounted device was demoted"
    n1.succeed(f"umount {MNT} && drbdadm secondary {NAME}")

with subtest("failover: another replica takes over with the data intact"):
    start_hold(n2)
    wait_role(n2, "Primary")
    n2.succeed(f"mount /dev/drbd0 {MNT}")
    assert n2.succeed(f"sha256sum {MNT}/data").split()[0] == reference, "data changed across failover"
    for m in (n1, n2, n3):
        assert roles(observe(m, IDS)) == {
            "n1": ("Secondary", True), "n2": ("Primary", True), "n3": ("Secondary", True),
        }, f"{m.name} sees {observe(m, IDS)}"

with subtest("a second primary is refused until the first lets go"):
    start_hold(n3)
    time.sleep(6)
    assert role(n3) == "Secondary" and role(n2) == "Primary", "two primaries, or n2 lost the role"
    assert "not possible yet" in hold_log(n3), hold_log(n3)
    revoke(n2)
    assert not mounted(n2)
    wait_role(n3, "Primary")
    assert role(n2) == "Secondary"
    n3.succeed(f"mount /dev/drbd0 {MNT}")
    assert n3.succeed(f"sha256sum {MNT}/data").split()[0] == reference

with subtest("quorum lost: reads and writes fail, and the gate leaves the primary alone"):
    partition(n3)
    wait_quorum(n3, False)
    assert role(n3) == "Primary" and hold_active(n3), "the gate reacted to quorum loss"
    assert n3.execute("dd if=/dev/drbd0 of=/dev/null bs=4k count=1 iflag=direct 2>&1")[0] != 0, \
        "a read succeeded without quorum"
    assert n3.execute("dd if=/dev/zero of=/dev/drbd0 bs=4k count=1 oflag=direct 2>&1")[0] != 0, \
        "a write succeeded without quorum"
    assert has_quorum(n1) and has_quorum(n2), "the majority side lost quorum"
    cut = observe(n3, IDS)
    assert not cut["quorum"] and cut["n3"]["Role"] == "Primary" and not cut["n3"]["Healthy"], cut
    assert cut["n1"]["Role"] == "" and cut["n2"]["Role"] == "", f"a peer cannot be seen through the cut: {cut}"
    assert observe(n1, IDS)["n3"]["Role"] == "", "the majority still sees the partitioned primary"

with subtest("quorum returns: I/O resumes and the data is intact"):
    heal(n3)
    wait_quorum(n3, True)
    n3.succeed("dd if=/dev/drbd0 of=/dev/null bs=4k count=1 iflag=direct")
    n3.succeed("echo 3 > /proc/sys/vm/drop_caches")
    assert n3.succeed(f"sha256sum {MNT}/data").split()[0] == reference

with subtest("shutdown demotes: SIGTERM unmounts and steps down"):
    n3.succeed(f"systemctl stop hold-{NAME}")
    assert not mounted(n3), "shutdown left the device attached"
    assert role(n3) == "Secondary", "shutdown left the volume primary"

with subtest("a killed agent leaves the kernel primary (the gap the agent unit must close)"):
    n3.succeed(f"drbdadm primary {NAME}")
    start_hold(n3)
    time.sleep(2)
    n3.succeed(f"systemctl kill -s KILL hold-{NAME}")
    time.sleep(2)
    assert role(n3) == "Primary", "kill unexpectedly demoted"
    n3.execute(f"systemctl reset-failed hold-{NAME}")
    n3.succeed(f"drbdadm secondary {NAME}")

with subtest("everything settles back to three UpToDate secondaries"):
    for h in NODES:
        wait_until(by_name[h], all_uptodate(PEERS[h]), "UpToDate", timeout=120)
        assert role(by_name[h]) == "Secondary"
