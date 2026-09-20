"""vol-drbd-status-capture testScript body (Phase 1 B1).

Drives DRBD into each state the status parser must understand and copies the
real `drbdsetup status --json` output to $out/fixtures. Nothing is asserted
beyond the harness reaching each state; the fixtures are the product.
"""

import time

MACHINES = [n1, n2, n3]
IPS = {"n1": "192.168.1.1", "n2": "192.168.1.2", "n3": "192.168.1.3"}
STATUS = "drbdsetup status {res} --json --verbose --statistics"


def res_file(name, minor, port, disk, members, quorum):
    on = "".join(f"  on {h} {{ node-id {i}; address {IPS[h]}:{port}; }}\n" for h, i in members.items())
    opts = "quorum majority; on-no-quorum io-error;" if quorum else ""
    return (
        f"resource {name} {{\n  device /dev/drbd{minor} minor {minor};\n  disk {disk};\n  meta-disk internal;\n"
        "  net { protocol C; ping-int 2; ping-timeout 10; timeout 30; }\n"
        "  disk { resync-rate 2M; c-plan-ahead 0; }\n"
        f"  options {{ {opts} }}\n{on}"
        f"  connection-mesh {{ hosts {' '.join(members)}; }}\n}}\n"
    )


def write_conf(m, name, minor, port, disk, members, quorum):
    conf = res_file(name, minor, port, disk, members, quorum)
    m.succeed("mkdir -p /etc/drbd.d /var/lib/drbd /tmp/fx")
    m.succeed(f"cat > /etc/drbd.d/{name}.res <<'EOF'\n{conf}EOF")


def bring_up(name, minor, port, disk, members, quorum):
    for h in members:
        m = globals()[h]
        write_conf(m, name, minor, port, disk, members, quorum)
        m.succeed(f"drbdadm create-md --force --max-peers=7 {name}")
        m.succeed(f"drbdadm up {name}")


def status_json(m, res):
    return m.execute(STATUS.format(res=res) + " 2>&1")[1]


def wait_status(m, res, needle, timeout=90):
    deadline = time.time() + timeout
    out = ""
    while time.time() < deadline:
        out = status_json(m, res)
        if needle in out:
            return out
        time.sleep(0.5)
    raise Exception(f"{m.name}: {needle!r} never appeared in status of {res}:\n{out}")


def capture(m, res, name):
    """Save the current status of `res` on `m` as fixtures/<name>.<node>.json."""
    path = f"/tmp/fx/{name}.{m.name}.json"
    m.succeed(f"{STATUS.format(res=res)} > {path}")
    m.copy_from_vm(path, "fixtures")
    print(f"CAPTURED {name}.{m.name}: {m.succeed(f'wc -c < {path}').strip()} bytes")


def write_mb(m, dev, count):
    m.succeed(f"dd if=/dev/urandom of={dev} bs=1M count={count} oflag=direct conv=notrunc")


start_all()
for m in MACHINES:
    m.wait_for_unit("multi-user.target")
    m.succeed("modprobe drbd")
print("drbd module", n1.succeed("cat /sys/module/drbd/version").strip())
print("drbdadm --json:", n1.execute("drbdadm status r0 --json 2>&1")[1][:200])

with subtest("split-brain on a two-node resource without quorum"):
    pair = {"n1": 0, "n2": 1}
    bring_up("r1", 1, 7790, "/dev/vdc", pair, quorum=False)
    n1.succeed("drbdadm primary --force r1")
    wait_status(n1, "r1", '"peer-disk-state": "UpToDate"')
    n1.succeed("drbdadm secondary r1")
    for m in (n1, n2):
        m.succeed("drbdadm disconnect r1")
    for m in (n1, n2):
        m.succeed("drbdadm primary r1")
        write_mb(m, "/dev/drbd1", 1)
        m.succeed("drbdadm secondary r1")
    for m in (n1, n2):
        m.succeed("drbdadm connect r1")
    time.sleep(10)
    capture(n1, "r1", "split-brain")
    capture(n2, "r1", "split-brain")
    n1.succeed("journalctl -k --no-pager | grep -i 'split-brain\\|unrelated' > /tmp/fx/split-brain.kernel.txt || true")
    n1.copy_from_vm("/tmp/fx/split-brain.kernel.txt", "fixtures")

with subtest("healthy three-replica resource"):
    trio = {"n1": 0, "n2": 1, "n3": 2}
    bring_up("r0", 0, 7789, "/dev/vdb", trio, quorum=True)
    n1.succeed("drbdadm primary --force r0")
    wait_status(n1, "r0", '"peer-disk-state": "UpToDate"')
    n1.succeed("drbdadm secondary r0")
    n1.succeed("drbdadm primary r0")
    for m in (n1, n2, n3):
        for _ in range(60):
            if status_json(m, "r0").count('"UpToDate"') >= 3:
                break
            time.sleep(1)
    capture(n1, "r0", "healthy-primary")
    capture(n2, "r0", "healthy-secondary")
    n1.execute("drbdsetup status nosuch --json > /tmp/fx/unconfigured.txt 2>&1; echo rc=$? >> /tmp/fx/unconfigured.txt")
    n1.copy_from_vm("/tmp/fx/unconfigured.txt", "fixtures")

with subtest("degraded: one replica lost, quorum kept"):
    write_mb(n1, "/dev/drbd0", 1)
    n3.crash()
    wait_status(n1, "r0", '"connection-state": "Connecting"')
    capture(n1, "r0", "degraded")
    write_mb(n1, "/dev/drbd0", 64)

with subtest("syncing: the lost replica returns and catches up under a rate limit"):
    n3.start()
    n3.wait_for_unit("multi-user.target")
    n3.succeed("modprobe drbd")
    write_conf(n3, "r0", 0, 7789, "/dev/vdb", trio, quorum=True)
    n3.succeed("drbdadm up r0")
    wait_status(n1, "r0", '"replication-state": "SyncSource"')
    capture(n1, "r0", "syncing-source")
    capture(n3, "r0", "syncing-target")
    wait_status(n1, "r0", '"replication-state": "Established"', 180)

with subtest("no quorum: both peers lost"):
    n2.crash()
    n3.crash()
    wait_status(n1, "r0", '"quorum": false')
    capture(n1, "r0", "quorum-lost")

print("VOL-DRBD-STATUS-CAPTURE COMPLETED")
