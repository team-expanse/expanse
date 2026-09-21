"""vol-drbd-probe testScript body (Phase 1 B4).

Records how drbdadm behaves on the questions the volume runtime must answer
without guessing: is metadata present, is the running config in sync, and
which commands are safe to repeat. One node, one peer that never answers.
Observations only; nothing is asserted about the answers.
"""

DEV = "/dev/vdb"


def res_file(members):
    on = "".join(
        f"  on {h} {{ node-id {i}; address 192.168.1.{i + 1}:7789; }}\n" for h, i in members.items()
    )
    return (
        "resource r0 {\n"
        f"  device /dev/drbd0 minor 0;\n  disk {DEV};\n  meta-disk internal;\n"
        "  net { protocol C; }\n"
        f"{on}"
        f"  connection-mesh {{ hosts {' '.join(members)}; }}\n"
        "}\n"
    )


def conf(members):
    n1.succeed(f"cat > /etc/drbd.d/r0.res <<'EOF'\n{res_file(members)}EOF")


def probe(label, cmd):
    rc, out = n1.execute(f"{cmd} 2>&1")
    print(f"PROBE [{label}] `{cmd}` rc={rc}\n{out.strip()}\n---")


start_all()
n1.wait_for_unit("multi-user.target")
n1.succeed("mkdir -p /etc/drbd.d /var/lib/drbd && modprobe drbd")
conf({"n1": 0, "n2": 1})

probe("blank md", "drbdadm dump-md r0")
probe("blank md", "drbdadm get-gi r0")
probe("blank md, non-interactive", "drbdadm dump-md --force r0")
n1.succeed("drbdadm create-md --force --max-peers=7 r0")
probe("with md", "drbdadm dump-md r0")
probe("with md", "drbdadm get-gi r0")
probe("md, resource down", "drbdadm dstate r0")

probe("dry-run adjust, resource down", "drbdadm -d adjust r0")
probe("first up", "drbdadm up r0")
probe("second up", "drbdadm up r0")
probe("in-sync dry-run adjust", "drbdadm -d adjust r0")
probe("in-sync real adjust", "drbdadm adjust r0")
probe("status", "drbdadm status r0")

conf({"n1": 0})
probe("drift: peer removed from file, dry-run", "drbdadm -d adjust r0")
probe("drift: apply", "drbdadm adjust r0")
probe("after drift dry-run", "drbdadm -d adjust r0")
probe("forget-peer 1 (was removed)", "drbdsetup forget-peer r0 1")
probe("forget-peer 1 again", "drbdsetup forget-peer r0 1")
probe("forget-peer 9 (never known)", "drbdsetup forget-peer r0 9")
probe("forget-peer own id 0", "drbdsetup forget-peer r0 0")

conf({"n1": 0, "n2": 1})
probe("peer re-added, dry-run", "drbdadm -d adjust r0")
probe("peer re-added, apply", "drbdadm adjust r0")
probe("resize no-op", "drbdadm resize r0")
probe("down", "drbdadm down r0")
probe("down again", "drbdadm down r0")
probe("md survives down", "drbdadm dump-md r0")
probe("wipe then dump-md", f"dd if=/dev/zero of={DEV} bs=1M count=4 conv=notrunc && drbdadm dump-md r0")
