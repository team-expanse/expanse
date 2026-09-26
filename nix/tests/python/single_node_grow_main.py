"""cluster-single-node-grow (Phase 12 B3): a volume made on a one-node cluster grows to its
target of 3 as nodes join, under continuous acked writes, with zero acked-write loss.

n1 forms alone; a default volume lands 1/3. A writer on n1 acks records to a ledger (the
vol-durability recorder) throughout. n2 joins: the volume gains its first peer (quorum still
off, 2/3). n3 joins: 3/3, Healthy, and DRBD quorum turns on (R1: the flip must not error the
primary's I/O). Every acked record is then verified on n1 and, after a move-primary, on n3.

Runs after cluster-common.py, vol_cluster.py and single_node_common.py; RECORDER is the
recorder in the store, REC where it is copied on each node.
"""

SIZE_MIB = 256  # > WRITE_LIMIT (60000) x 4 KiB records, so the writer never runs off the end
LEDGER_PORT = 9440
LEDGER = "/root/ledger-grow"
# DRBD throttles resync toward c-min-rate (250 KiB/s) under app I/O; a lighter writer lets a
# full first-peer sync finish in minutes (found live: at 2 ms, 256 MiB took over 5 min to 87%).
WRITER_PAUSE_MS = 20
SYNC_BUDGET_S = 600
MIN_ACKED = 20


def rec(m, args):
    return m.execute(f"{REC} {args}")


def acked_count():
    return int(rec(n1, f"highest {LEDGER}")[1].strip()) + 1


def wait_writer_advances(what):
    before = acked_count()
    wait_for(lambda: acked_count() > before + MIN_ACKED, f"the writer to keep acking {what}", 120)
    assert n1.succeed("systemctl is-active dur-writer || true").strip() == "active", "the writer died"
    print(f"writer acked {acked_count()} records {what}")


def drbd_quorum(res):
    found = re.search(r"^\s*quorum\s+(\w+);", n1.succeed(f"drbdsetup show --show-defaults {res}"), re.M)
    return found.group(1) if found else ""


def peers_up_to_date(res):
    return drbd_status(n1, res).count("peer-disk:UpToDate")


def members_are(name, nodes, state):
    row = volume_row(n1, name)
    return row is not None and row["nodes"] == nodes and row["state"] == state


def assert_no_loss(m, acked):
    rc, out = rec(m, f"verify {device_of(m)} {acked}")
    assert rc == 0, f"ACKED WRITE LOST on {m.name} ({acked} acked): {out}"


def join(m, token):
    m.start()
    m.wait_for_unit("multi-user.target")
    m.succeed("systemctl stop expansed.service")
    m.copy_from_host(RECORDER, REC)
    join_and_start(m, token)


def dump_on_failure(res):
    print(status(n1))
    print(n1.execute("expanse ctl volume list 2>&1")[1])
    print(drbd_status(n1, res))
    print(f"highest acked record: {rec(n1, f'highest {LEDGER}')[1].strip()}")
    print(n1.execute("journalctl -u expansed.service --no-pager | grep -iE 'replica|volume|drbd' | tail -40")[1])


res = ""
try:
    with subtest("a default volume on a one-node cluster is 1/3"):
        form_single("grow", uses=2)
        n1.copy_from_host(RECORDER, REC)
        n1.succeed(f"expanse ctl volume create grow --size {SIZE_MIB}Mi")
        wait_for(lambda: members_are("grow", ["n1"], "underreplicated"), "grow to be placed 1/3", 120)
        n1.wait_until_succeeds("drbdadm status | grep -q '^vol-'", timeout=120)
        res = n1.succeed("drbdadm status | head -1 | cut -d' ' -f1").strip()
        wait_for(lambda: primaries(res, [n1]) == [n1], "n1 to be primary", 120)
        assert drbd_quorum(res) == "off", f"one replica runs quorum {drbd_quorum(res)!r}, want off"

    with subtest("continuous acked writes start on the lone replica"):
        n1.succeed(f"systemd-run --unit=dur-ledger {REC} ledger {LEDGER_PORT} {LEDGER}")
        n1.wait_until_succeeds(f"ss -ltn | grep -q :{LEDGER_PORT}", timeout=30)
        n1.succeed(f"systemd-run --unit=dur-writer {REC} write {device_of(n1)} 127.0.0.1 {LEDGER_PORT} 0 {WRITER_PAUSE_MS}")
        wait_for(lambda: acked_count() >= MIN_ACKED, "the first acked records", 120)

    token = n1.succeed("cat /root/join-token").strip()

    with subtest("n2 joins: the volume gains its first peer and stays writable"):
        join(n2, token)
        wait_quorum("2/2", 120)
        wait_for(lambda: members_are("grow", ["n1", "n2"], "underreplicated") and peers_up_to_date(res) == 1,
                 "grow to be 2/3 with n2 UpToDate", SYNC_BUDGET_S)
        assert drbd_quorum(res) == "off", f"two replicas run quorum {drbd_quorum(res)!r}, want off"
        wait_writer_advances("through n2's join")

    with subtest("n3 joins: 3/3, Healthy, quorum on, and the primary never errors"):
        join(n3, token)
        wait_quorum("3/2", 120)
        wait_for(lambda: members_are("grow", ["n1", "n2", "n3"], "healthy") and peers_up_to_date(res) == 2,
                 "grow to be 3/3 Healthy", SYNC_BUDGET_S)
        wait_for(lambda: drbd_quorum(res) == "majority", "DRBD quorum to turn on", 60)
        wait_writer_advances("through the quorum flip")

    with subtest("every acked write is on n1, and on n3 after a move-primary"):
        n1.succeed("systemctl stop dur-writer")
        acked = acked_count()
        assert_no_loss(n1, acked)
        n1.succeed("expanse ctl volume move-primary grow --to n3")
        wait_for(lambda: primaries(res) == [n3], "n3 to become primary", 120)
        assert_no_loss(n3, acked)
        print(f"SINGLE-NODE-GROW PASSED: {acked} acked records intact across 1 -> 2 -> 3")
except Exception:
    dump_on_failure(res)
    raise
