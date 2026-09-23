"""iscsi-lio-drbd-secondary-probe testScript body (Phase 4 D1).

Two nodes, one DRBD resource, n1 Primary / n2 Secondary. Probes whether
targetcli-fb (rtslib-fb) can create a block backstore against n2's Secondary
device at all, and if so, what actually happens when an initiator drives real
I/O through it. Observations only; nothing is asserted about the answers.
"""

DEV = "/dev/drbd0"


def probe(node, label, cmd):
    rc, out = node.execute(f"{cmd} 2>&1")
    print(f"PROBE [{label}] `{cmd}` rc={rc}\n{out.strip()}\n---")
    return rc, out


start_all()
n1.wait_for_unit("multi-user.target")
n2.wait_for_unit("multi-user.target")

for n in (n1, n2):
    n.succeed("mkdir -p /etc/drbd.d /var/lib/drbd && modprobe drbd")
n1.succeed("drbdadm create-md --force --max-peers=7 r0")
n2.succeed("drbdadm create-md --force --max-peers=7 r0")
n1.succeed("drbdadm up r0")
n2.succeed("drbdadm up r0")
n1.succeed("drbdadm primary --force r0")
n1.wait_until_succeeds("drbdadm status r0 | grep -q 'peer-disk:UpToDate'", timeout=60)

probe(n1, "n1 role", "drbdadm status r0")
probe(n2, "n2 role", "drbdadm status r0")

# Sanity: primary-side backstore creation, the known-good case.
probe(n1, "n1 (Primary) getsize64", f"blockdev --getsize64 {DEV}")
rc, _ = probe(
    n1, "n1 (Primary) targetcli backstore create",
    f"targetcli /backstores/block create name=probe1 dev={DEV}",
)
print(f"n1 Primary backstore create: {'OK' if rc == 0 else 'FAILED'}")

# The actual question: does Secondary refuse at open()/size-query time, or
# only at real data I/O?
probe(n2, "n2 (Secondary) getsize64", f"blockdev --getsize64 {DEV}")
probe(n2, "n2 (Secondary) dd read 4k", f"dd if={DEV} of=/dev/null bs=4k count=1")
rc, _ = probe(
    n2, "n2 (Secondary) targetcli backstore create",
    f"targetcli /backstores/block create name=probe2 dev={DEV}",
)
print(f"n2 Secondary backstore create: {'OK' if rc == 0 else 'FAILED'}")

if rc == 0:
    # Backstore object exists -- does LIO's own info command need to read
    # the device, and does exporting it as a target/LUN succeed?
    probe(n2, "n2 backstore info", "targetcli /backstores/block/probe2 info")
    rc2, _ = probe(
        n2, "n2 create target+LUN over the Secondary-backed backstore",
        "targetcli /iscsi create iqn.2026-09.io.expanse:probe && "
        "targetcli /iscsi/iqn.2026-09.io.expanse:probe/tpg1/luns create /backstores/block/probe2",
    )
    print(f"n2 Secondary target+LUN create: {'OK' if rc2 == 0 else 'FAILED'}")

print("ISCSI-LIO-DRBD-SECONDARY-PROBE DONE")
