"""vol-resync-rate probe: how DRBD's c-min-rate trades a full resync against a busy writer.

n1 is primary of a raw two-node resource shaped like Expanse's (protocol C) and runs the
grow test's fsync-per-4 KiB writer. Each round wipes n2's metadata so it takes a full sync
from n1 under that writer, with one c-min-rate on both nodes, and reports the sync time and
the writer's acked rate. Observations only; RATE lines are the result.

Runs with RECORDER (the vol-durability recorder) and REC (its copy on n1) defined.
"""

import re
import time

DEV = "/dev/vdb"
DRBD_DEV = "/dev/drbd0"
LEDGER_PORT = 9440
WRITER_PAUSE_MS = 2  # the pace that stalled the grow test's first sync (Phase 12 B2)
ROUND_BUDGET_S = 240
SETTINGS = ["250k", "4M", "16M", "64M", "0"]  # 250k is DRBD's default; 0 turns throttling off


def res_file(min_rate):
    return (
        "resource r0 {\n"
        f"  device {DRBD_DEV} minor 0;\n  disk {DEV};\n  meta-disk internal;\n"
        f"  disk {{ c-min-rate {min_rate}; }}\n"
        "  net { protocol C; verify-alg sha1; }\n"
        "  options { auto-promote no; quorum off; }\n"
        "  on n1 { node-id 0; address 192.168.1.1:7789; }\n"
        "  on n2 { node-id 1; address 192.168.1.2:7789; }\n"
        "  connection-mesh { hosts n1 n2; }\n"
        "}\n"
    )


def configure(m, min_rate):
    m.succeed(f"cat > /etc/drbd.d/r0.res <<'EOF'\n{res_file(min_rate)}EOF")


def applied_min_rate(m):
    found = re.search(r"c-min-rate\s+(\d+)", m.succeed("drbdsetup show --show-defaults r0"))
    return found.group(1) if found else "?"


def sync_done_pct(m):
    """Percent of the resync n1 reports done toward n2; 100 once n2 is UpToDate."""
    out = n1.succeed("drbdsetup status r0 --verbose --statistics")
    if "peer-disk:UpToDate" in out:
        return 100.0
    found = re.search(r"done:([\d.]+)", out)
    return float(found.group(1)) if found else 0.0


def acked(ledger):
    return int(n1.succeed(f"{REC} highest {ledger}").strip()) + 1


def start_writer(ledger):
    n1.succeed(f"systemd-run --unit=rate-ledger {REC} ledger {LEDGER_PORT} {ledger}")
    n1.wait_until_succeeds(f"ss -ltn | grep -q :{LEDGER_PORT}", timeout=30)
    n1.succeed(f"systemd-run --unit=rate-writer {REC} write {DRBD_DEV} 127.0.0.1 {LEDGER_PORT} 0 {WRITER_PAUSE_MS}")


def stop_writer():
    n1.execute("systemctl stop rate-writer rate-ledger; systemctl reset-failed rate-writer rate-ledger")


def writer_rate(ledger, seconds):
    before, start = acked(ledger), time.time()
    time.sleep(seconds)
    return (acked(ledger) - before) / (time.time() - start)


def fresh_peer():
    """n2 comes back with blank metadata, so n1 must copy it everything."""
    n2.execute("drbdadm down r0")
    n2.succeed(f"wipefs -a {DEV} && dd if=/dev/zero of={DEV} bs=1M count=1 oflag=direct")
    n2.succeed("drbdadm create-md --force r0 && drbdadm up r0")


def timed_sync():
    """(seconds, percent done) until n2 is UpToDate or the budget runs out."""
    start = time.time()
    while time.time() - start < ROUND_BUDGET_S:
        pct = sync_done_pct(n1)
        if pct >= 100.0:
            return time.time() - start, 100.0
        time.sleep(2)
    return time.time() - start, sync_done_pct(n1)


def measure(min_rate, with_writer):
    for m in (n1, n2):
        configure(m, min_rate)
    n1.succeed("drbdadm adjust r0")
    ledger = f"/root/ledger-{min_rate}-{int(with_writer)}"
    if with_writer:
        start_writer(ledger)
        n1.wait_until_succeeds(f"test $({REC} highest {ledger}) -ge 20", timeout=60)
    before, t0 = acked(ledger), time.time()
    fresh_peer()
    seconds, pct = timed_sync()
    rate = (acked(ledger) - before) / (time.time() - t0)
    stop_writer()
    print(f"RATE c-min-rate={min_rate} (applied {applied_min_rate(n1)}) writer={'on' if with_writer else 'off'}: "
          f"sync {pct:.1f}% in {seconds:.0f}s, writer {rate:.0f} acked/s")


start_all()
for m in (n1, n2):
    m.wait_for_unit("multi-user.target")
    m.succeed("mkdir -p /etc/drbd.d /var/lib/drbd && modprobe drbd")
    configure(m, SETTINGS[0])
n1.copy_from_host(RECORDER, REC)
n1.succeed("drbdadm create-md --force r0 && drbdadm up r0 && drbdadm primary --force r0")

with subtest("baseline: the writer's rate with n2 connected and in sync"):
    fresh_peer()
    n1.wait_until_succeeds("drbdsetup status r0 | grep -q peer-disk:UpToDate", timeout=300)
    start_writer("/root/ledger-baseline")
    print(f"RATE baseline: writer {writer_rate('/root/ledger-baseline', 30):.0f} acked/s, peer in sync")
    stop_writer()

with subtest("baseline: a full sync with no writer"):
    measure(SETTINGS[0], with_writer=False)

for setting in SETTINGS:
    with subtest(f"a full sync under the writer, c-min-rate {setting}"):
        measure(setting, with_writer=True)

print(n1.succeed("uname -r; drbdadm --version | head -3"))
