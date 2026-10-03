"""vol-drbd-tiebreaker-probe testScript body (bug #2: split brain on 2-replica volumes).

Question: can a third, diskless DRBD member give a live 2-replica resource a majority quorum,
so a cut-off primary stops writing instead of diverging? Each step asserts what the tiebreaker
design depends on and records what DRBD did; the findings table prints at the end.
"""

import time

DISKFUL = [n1, n2]
TIE = n3
DEV = "/dev/vdb"
DRBD = "/dev/drbd0"
IPS = {"n1": "192.168.1.1", "n2": "192.168.1.2", "n3": "192.168.1.3"}
# The on-section forms a diskless member might take; the first that drbdadm accepts is used.
DISKLESS_FORMS = {
    "disk none": "disk none;",
    "volume 0 disk none": "volume 0 { device minor 0; disk none; meta-disk internal; }",
}
findings = []


def note(experiment, observed):
    findings.append((experiment, observed))
    print(f"FINDING [{experiment}] {observed}")


def sh(m, cmd):
    rc, out = m.execute(f"{cmd} 2>&1")
    return rc, out.strip()


def res_file(members, diskless=(), form="disk none"):
    """members: {host: node_id}; hosts in diskless carry no backing disk. Mirrors drbd.Resource.Render."""
    quorum = "quorum majority; on-no-quorum io-error;" if len(members) >= 3 else "quorum off;"
    on = ""
    for h, i in members.items():
        extra = f" {DISKLESS_FORMS[form]}" if h in diskless else ""
        on += f"  on {h} {{ node-id {i}; address {IPS[h]}:7789;{extra} }}\n"
    return (
        "resource r0 {\n"
        f"  device {DRBD} minor 0;\n  disk {DEV};\n  meta-disk internal;\n"
        "  net { protocol C; verify-alg sha1; after-sb-0pri disconnect; after-sb-1pri disconnect;"
        " after-sb-2pri disconnect; rr-conflict disconnect; }\n"
        f"  options {{ auto-promote no; {quorum} }}\n"
        "  handlers { split-brain \"/run/current-system/sw/bin/touch /var/tmp/split-brain\"; }\n"
        f"{on}"
        f"  connection-mesh {{ hosts {' '.join(members)}; }}\n"
        "}\n"
    )


def write_conf(m, members, diskless=(), form="disk none"):
    m.succeed("mkdir -p /etc/drbd.d")
    m.succeed(f"cat > /etc/drbd.d/r0.res <<'EOF'\n{res_file(members, diskless, form)}EOF")


def status(m):
    return sh(m, "drbdsetup status r0 --verbose")[1]


def shown(m):
    """drbdsetup show with its column padding collapsed, for substring checks."""
    return " ".join(sh(m, "drbdsetup show r0")[1].split())


def flat(text):
    return text.replace("\n", " | ")


def wait_until(pred, what, timeout):
    deadline = time.time() + timeout
    while time.time() < deadline:
        if pred():
            return
        time.sleep(0.5)
    raise AssertionError(f"timed out waiting for {what}")


def has_quorum(m):
    return "quorum:no" not in status(m)


def start_io_loop(m):
    m.succeed("rm -f /tmp/io.ok /tmp/io.fail /tmp/io.stop; touch /tmp/io.ok /tmp/io.fail")
    m.succeed(
        "nohup sh -c 'while [ ! -e /tmp/io.stop ]; do "
        f"if timeout 5 dd if=/dev/urandom of={DRBD} bs=4k count=1 seek=$((RANDOM%2000+2048)) oflag=direct conv=notrunc 2>/dev/null; "
        "then echo x >> /tmp/io.ok; else echo x >> /tmp/io.fail; fi; sleep 0.05; done' >/dev/null 2>&1 &"
    )


def io_counts(m):
    return int(m.succeed("wc -l < /tmp/io.ok").strip()), int(m.succeed("wc -l < /tmp/io.fail").strip())


def stop_io_loop(m):
    m.succeed("touch /tmp/io.stop; sleep 6")
    return io_counts(m)


def verify_clean(m):
    """Online verify; True when out-of-sync is zero against every diskful peer."""
    m.succeed("drbdadm verify r0")
    wait_until(lambda: "Verify" not in status(m), "verify to finish", 120)
    out = m.succeed("drbdsetup status r0 --verbose --statistics")
    oos = [line.split("out-of-sync:")[1].split()[0] for line in out.splitlines() if "out-of-sync:" in line]
    return all(v == "0" for v in oos), oos


def pick_diskless_form():
    """The first on-section form that drbdadm parses for the diskless host itself."""
    members = {"n1": 0, "n2": 1, "n3": 2}
    for form in DISKLESS_FORMS:
        write_conf(TIE, members, {"n3"}, form)
        rc, out = sh(TIE, "drbdadm dump r0")
        note(f"syntax '{form}' dump", f"rc={rc}")
        if rc == 0:
            rc, out = sh(TIE, "drbdadm -d up r0")
            note(f"syntax '{form}' dry-run up", f"rc={rc} out={flat(out)!r}")
            if rc == 0 and "attach" not in out:
                return form
    raise AssertionError("no diskless form was accepted")


start_all()
for m in (n1, n2, n3):
    m.wait_for_unit("multi-user.target")
    m.succeed("modprobe drbd")
version = n1.succeed("cat /sys/module/drbd/version").strip()
note("versions", f"module {version}; {flat(n1.succeed('drbdadm --version 2>&1 | head -3'))}")

with subtest("a diskless member has a config form drbdadm accepts"):
    FORM = pick_diskless_form()
    note("chosen form", FORM)

with subtest("baseline: two diskful replicas with quorum off and a primary writing"):
    TWO = {"n1": 0, "n2": 1}
    for m in DISKFUL:
        m.succeed("mkdir -p /var/lib/drbd")
        write_conf(m, TWO)
        m.succeed("drbdadm create-md --force --max-peers=7 r0")
        m.succeed("drbdadm up r0")
    n1.succeed("drbdadm primary --force r0")
    wait_until(lambda: "peer-disk:UpToDate" in status(n1), "n2 to be UpToDate", 180)
    start_io_loop(n1)
    time.sleep(2)

THREE = {"n1": 0, "n2": 1, "n3": 2}

with subtest("adding the tiebreaker to the live resource turns quorum on without disturbing I/O"):
    for m in DISKFUL:
        write_conf(m, THREE, {"n3"}, FORM)
        rc, out = sh(m, "drbdadm adjust r0")
        note(f"adjust 2->3 on {m.name}", f"rc={rc} out={out!r}")
        assert rc == 0, f"adjust failed on {m.name}: {out}"
    write_conf(TIE, THREE, {"n3"}, FORM)
    TIE.succeed("drbdadm up r0")
    wait_until(lambda: status(n1).count("connection:Connected") == 2, "n1 to connect to both peers", 60)
    note("n1 options after adjust", flat(sh(n1, "drbdsetup show r0 | grep -A4 options")[1]))
    note("n1 status with tiebreaker", flat(status(n1)))
    note("tiebreaker status", flat(status(TIE)))
    assert "peer-disk:Diskless" in status(n1), "n1 does not see n3 as a diskless peer"
    assert "quorum majority" in shown(n1), "quorum was not switched on live"
    time.sleep(3)
    ok, fail = io_counts(n1)
    note("io across the adjust", f"ok={ok} fail={fail}")
    assert fail == 0, f"{fail} writes failed while adding the tiebreaker"

with subtest("a cut-off primary loses quorum and stops writing; survivor plus tiebreaker promote"):
    before_ok, _ = io_counts(n1)
    began = time.time()
    n1.block()
    wait_until(lambda: not has_quorum(n1), "n1 to lose quorum", 60)
    note("n1 lost quorum after", f"{time.time() - began:.1f}s")
    wait_until(lambda: io_counts(n1)[1] > 0, "n1 writes to fail", 60)
    time.sleep(3)
    ok_mid, _ = io_counts(n1)
    time.sleep(5)
    ok_late, fail_late = io_counts(n1)
    note("n1 io while cut off", f"ok before cut={before_ok} ok soon after={ok_mid} ok 5s later={ok_late} fail={fail_late}")
    assert ok_late == ok_mid, "n1 kept completing writes without quorum"
    wait_until(lambda: has_quorum(n2), "n2 to keep quorum", 60)
    rc, out = sh(n2, "drbdadm primary r0")
    note("n2 promote while n1 is cut off", f"rc={rc} out={out!r} after {time.time() - began:.1f}s")
    assert rc == 0, f"n2 could not promote: {out}"
    n2.succeed(f"dd if=/dev/urandom of={DRBD} bs=4k count=256 seek=4096 oflag=direct conv=notrunc")
    stop_io_loop(n1)
    note("n1 demote while cut off", str(sh(n1, "drbdadm secondary r0")))
    n1.unblock()

with subtest("the heal reconnects without a split brain and resyncs the old primary"):
    wait_until(lambda: status(n2).count("connection:Connected") == 2, "n2 to reconnect to both peers", 120)
    wait_until(lambda: "peer-disk:UpToDate" in status(n2) and "Sync" not in status(n2), "n1 to resync", 180)
    note("status after heal", flat(status(n2)))
    for m in (n1, n2, n3):
        assert m.execute("test -e /var/tmp/split-brain")[0] != 0, f"{m.name} saw a split brain"
        assert "StandAlone" not in status(m), f"{m.name} is StandAlone"
    clean, oos = verify_clean(n2)
    note("verify after heal", f"{clean} out-of-sync={oos}")
    assert clean, f"replicas differ after heal: {oos}"

with subtest("losing only the tiebreaker keeps the diskful pair writing"):
    start_io_loop(n2)
    TIE.crash()
    time.sleep(15)
    ok, fail = io_counts(n2)
    note("io after tiebreaker crash", f"ok={ok} fail={fail} quorum={has_quorum(n2)}")
    assert has_quorum(n2) and fail == 0, "the pair lost quorum when only the tiebreaker went away"

with subtest("dropping the tiebreaker live turns quorum back off"):
    for m in DISKFUL:
        write_conf(m, TWO)
        rc, out = sh(m, "drbdadm adjust r0")
        note(f"adjust 3->2 on {m.name}", f"rc={rc} out={out!r}")
        assert rc == 0, f"adjust failed on {m.name}: {out}"
    note("forget tiebreaker id 2", str(sh(n2, "drbdsetup forget-peer r0 2")))
    note("n2 options after drop", flat(sh(n2, "drbdsetup show r0 | grep -A4 options")[1]))
    assert "quorum majority" not in shown(n2), "quorum stayed on after the drop"
    time.sleep(3)
    ok, fail = stop_io_loop(n2)
    note("io across the drop", f"ok={ok} fail={fail}")
    assert fail == 0, "writes failed while dropping the tiebreaker"

with subtest("a tiebreaker can be added again under the same node-id after a forget"):
    TIE.start()
    TIE.wait_for_unit("multi-user.target")
    TIE.succeed("modprobe drbd")
    for m in DISKFUL:
        write_conf(m, THREE, {"n3"}, FORM)
        rc, out = sh(m, "drbdadm adjust r0")
        note(f"re-add adjust on {m.name}", f"rc={rc} out={out!r}")
        assert rc == 0, f"re-add adjust failed on {m.name}: {out}"
    write_conf(TIE, THREE, {"n3"}, FORM)
    TIE.succeed("drbdadm up r0")
    wait_until(lambda: status(n2).count("connection:Connected") == 2, "n2 to connect to both peers", 60)
    assert has_quorum(n2) and "quorum majority" in shown(n2)

with subtest("observation: the heal while the cut-off node is still Primary"):
    start_io_loop(n2)
    n2.block()
    wait_until(lambda: not has_quorum(n2), "n2 to lose quorum", 60)
    wait_until(lambda: has_quorum(n1), "n1 to keep quorum", 60)
    note("n1 promote", str(sh(n1, "drbdadm primary r0")))
    n1.succeed(f"dd if=/dev/urandom of={DRBD} bs=4k count=256 seek=8192 oflag=direct conv=notrunc")
    n2.unblock()
    time.sleep(15)
    note("both Primary at heal: n1", flat(status(n1)))
    note("both Primary at heal: n2", flat(status(n2)))
    note("split-brain markers", str([m.name for m in (n1, n2, n3) if m.execute("test -e /var/tmp/split-brain")[0] == 0]))
    stop_io_loop(n2)
    note("n2 demote after heal", str(sh(n2, "drbdadm secondary r0")))
    time.sleep(10)
    note("status after n2 demotes", flat(status(n1)))

print("=" * 72)
for experiment, observed in findings:
    print(f"{experiment}: {observed}")
print("=" * 72)
print("VOL-DRBD-TIEBREAKER-PROBE COMPLETED")
