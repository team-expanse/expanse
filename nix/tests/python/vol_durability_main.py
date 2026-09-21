"""vol-durability (X1, the Phase 1 release gate): no acked write is lost across hard crashes.

Each iteration streams fsync'd records onto the volume's primary, hard-kills that VM
mid-stream (qemu quit, no flush), waits for a survivor to take over, and requires every
acked record on the new primary. The crashed node is then restored and, once resynced,
every replica must hold every acked record and all three must be byte-identical.

A record is acked when its fsync returned and an off-node ledger stored its sequence
number, so the ledger never claims more than the volume was told (see vol_durability_rec.py).
A qemu kill loses no host-side disk cache, so it tests replication and failover, not the
disks' own flush behaviour.

Runs after cluster-common.py and vol_cluster.py. EXPANSE_DURABILITY_ITERS overrides the count.
"""

import os
import random

ITERS = int(os.environ.get("EXPANSE_DURABILITY_ITERS", "20"))
SIZE_MIB = 256
LEDGER_PORT = 9440
MIN_ACKED_BEFORE_KILL = 20
WRITER_PAUSE_MS = 2
VG = "vg0"

rng = random.Random(0xD06AB1)  # deterministic, so a failing run reproduces


def rec(m, args):
    return m.execute(f"{REC} {args}")


def ledger_highest(host, path):
    return int(rec(host, f"highest {path}")[1].strip())


def dump_state(nodes):
    for m in nodes:
        print(f"[{m.name}] drbd:\n{drbd_status(m, res)}")
        print(f"[{m.name}] agent:\n{m.execute('journalctl -u expansed.service -n 15 --no-pager 2>&1')[1]}")


def wait_single_primary(nodes, timeout=300):
    try:
        wait_for(lambda: len(primaries(res, nodes)) == 1, "one primary", timeout)
    except Exception:
        dump_state(nodes)
        raise
    return primaries(res, nodes)[0]


def start_ledger(host, path):
    host.execute("systemctl stop dur-ledger 2>/dev/null")
    host.succeed(f"systemd-run --unit=dur-ledger {REC} ledger {LEDGER_PORT} {path}")
    try:
        host.wait_until_succeeds(f"ss -ltn | grep -q :{LEDGER_PORT}", timeout=30)
    except Exception:
        print(host.execute("journalctl -u dur-ledger.service -n 20 --no-pager 2>&1")[1])
        raise


def stream_until_killed(primary, host, path, start):
    """Write from record `start` until enough are acked, then hard-kill the primary."""
    dev = device_of(primary)
    primary.succeed(
        f"systemd-run --unit=dur-writer {REC} write {dev} {addr(host)} {LEDGER_PORT} {start} {WRITER_PAUSE_MS}"
    )
    wait_for(lambda: ledger_highest(host, path) >= start + MIN_ACKED_BEFORE_KILL, "records to ack", 120)
    time.sleep(rng.uniform(0, 1.0))
    primary.crash()


def assert_no_loss(m, dev, acked, what):
    rc, out = rec(m, f"verify {dev} {acked}")
    assert rc == 0, f"ACKED WRITE LOST on {what} ({acked} acked): {out} (release blocker)"


def restore(dead):
    dead.start()
    dead.wait_for_unit("multi-user.target", timeout=180)
    dead.wait_for_unit("expansed.service", timeout=120)
    wait_agent_ready(dead)
    wait_for(lambda: all(fully_replicated(m, res) for m in NODES), "every replica UpToDate", 300)


def assert_replicas_hold_and_agree(acked):
    lv = f"/dev/{VG}/{res}"
    sums = {}
    for m in NODES:
        assert_no_loss(m, lv, acked, f"replica {m.name}")
        sums[m.name] = rec(m, f"digest {lv} {SIZE_MIB * 1024 * 1024}")[1].strip()
    assert len(set(sums.values())) == 1, f"replicas diverged: {sums}"


form("voldur")

with subtest("volume created and replicated everywhere"):
    n1.succeed(f"expanse ctl volume create dur --size {SIZE_MIB}Mi --replication 3")
    for m in NODES:
        m.wait_until_succeeds("drbdadm status | grep -q '^vol-'", timeout=180)
    res = n1.succeed("drbdadm status | head -1 | cut -d' ' -f1").strip()
    wait_for(lambda: all(fully_replicated(m, res) for m in NODES), "every replica UpToDate", 300)
    wait_single_primary(NODES)

acked = 0
for it in range(ITERS):
    with subtest(f"iteration {it} ({acked} acked so far)"):
        primary = wait_single_primary(NODES)
        host = [m for m in NODES if m is not primary][it % 2]
        survivors = [m for m in NODES if m is not primary]
        ledger = f"/root/ledger-{it}"

        start_ledger(host, ledger)
        stream_until_killed(primary, host, ledger, acked)
        crashed_at = time.time()

        new_primary = wait_single_primary(survivors)
        failover_s = time.time() - crashed_at
        acked = max(acked, ledger_highest(host, ledger) + 1)
        print(f"iteration {it}: {primary.name} killed, {new_primary.name} primary after {failover_s:.0f}s, {acked} acked")

        assert_no_loss(new_primary, device_of(new_primary), acked, f"new primary {new_primary.name}")
        restore(primary)
        wait_single_primary(NODES)
        assert_replicas_hold_and_agree(acked)

with subtest("final state holds every acked record on every replica"):
    final = wait_single_primary(NODES)
    assert_no_loss(final, device_of(final), acked, f"final primary {final.name}")
    assert_replicas_hold_and_agree(acked)
    print(f"RELEASE GATE PASSED: {acked} acked records survived {ITERS} hard crashes, all replicas identical")
