"""vol-drbd-nodeid-spike testScript body (Phase 1 B3 / decision D5).

Question: when a replica is rebuilt on another node, can DRBD 9 take a new
node-id on a LIVE resource, or must the resource be recreated? Each
experiment records what DRBD actually did; nothing is asserted about the
answer, only that the harness itself worked. Results are printed as a
findings table at the end.
"""

import time

MACHINES = [n1, n2, n3, n4]
DEV = "/dev/vdb"
DRBD = "/dev/drbd0"
IPS = {"n1": "192.168.1.1", "n2": "192.168.1.2", "n3": "192.168.1.3", "n4": "192.168.1.4"}
findings = []
round_name = ""


def res_file(members):
    """members: {host: node_id}. Rendered like the B2 generator will."""
    on = "".join(
        f"  on {h} {{ node-id {i}; address {IPS[h]}:7789; }}\n" for h, i in members.items()
    )
    return (
        "resource r0 {\n"
        f"  device {DRBD} minor 0;\n  disk {DEV};\n  meta-disk internal;\n"
        "  net { protocol C; verify-alg sha1; ping-int 2; ping-timeout 10; timeout 30; }\n"
        "  options { quorum majority; on-no-quorum io-error; }\n"
        f"{on}"
        f"  connection-mesh {{ hosts {' '.join(members)}; }}\n"
        "}\n"
    )


def write_conf(m, members):
    m.succeed("mkdir -p /etc/drbd.d")
    m.succeed(f"cat > /etc/drbd.d/r0.res <<'EOF'\n{res_file(members)}EOF")


def sh(m, cmd):
    rc, out = m.execute(f"{cmd} 2>&1")
    return rc, out.strip()


def note(experiment, observed):
    findings.append((f"{round_name} {experiment}", observed))
    print(f"FINDING [{round_name} {experiment}] {observed}")


def status(m):
    return sh(m, "drbdadm status r0")[1]


def wait_for(m, needle, count, timeout):
    """Poll until `count` peers show `needle` and none is mid-replication."""
    deadline = time.time() + timeout
    out = ""
    while time.time() < deadline:
        out = status(m)
        if out.count(needle) >= count and "replication:" not in out:
            return True, out
        time.sleep(0.5)
    return False, out


def start_io_loop(m):
    m.succeed("rm -f /tmp/io.ok /tmp/io.fail; touch /tmp/io.ok /tmp/io.fail")
    m.succeed(
        "nohup sh -c 'while [ ! -e /tmp/io.stop ]; do "
        f"if dd if=/dev/urandom of={DRBD} bs=4k count=1 seek=$((RANDOM%2000+2048)) oflag=direct conv=notrunc 2>/dev/null; "
        "then echo x >> /tmp/io.ok; else echo x >> /tmp/io.fail; fi; sleep 0.05; done' >/dev/null 2>&1 &"
    )


def stop_io_loop(m):
    m.succeed("touch /tmp/io.stop; sleep 1; rm -f /tmp/io.stop")
    ok = int(m.succeed("wc -l < /tmp/io.ok").strip())
    fail = int(m.succeed("wc -l < /tmp/io.fail").strip())
    return ok, fail


def verify_clean(m, peers):
    """Online verify; True when out-of-sync is zero against every peer."""
    m.succeed("drbdadm verify r0")
    deadline = time.time() + 120
    while time.time() < deadline:
        if "VerifyS" not in status(m):
            break
        time.sleep(1)
    out = m.succeed("drbdsetup status r0 --verbose --statistics")
    oos = [line.split("out-of-sync:")[1].split()[0] for line in out.splitlines() if "out-of-sync:" in line]
    return all(v == "0" for v in oos) and len(oos) >= peers, oos



BASE = {"n1": 0, "n2": 1, "n3": 2}


def bring_up(max_peers):
    """Three UpToDate replicas; max_peers=None keeps drbdadm's default."""
    flag = f"--max-peers={max_peers} " if max_peers else ""
    for m in (n1, n2, n3):
        m.succeed("mkdir -p /var/lib/drbd")
        write_conf(m, BASE)
        m.succeed(f"drbdadm create-md --force {flag}r0")
        m.succeed("drbdadm up r0")
    n1.succeed("drbdadm primary --force r0")
    ok, out = wait_for(n1, "peer-disk:UpToDate", 2, 180)
    assert ok, f"baseline never UpToDate:\n{out}"
    n1.succeed(f"dd if=/dev/urandom of={DRBD} bs=1M count=8 oflag=direct conv=notrunc")


def reset_cluster():
    for m in (n3, n4):
        m.start()
        m.wait_for_unit("multi-user.target")
    for m in MACHINES:
        m.succeed("modprobe drbd")
        m.execute("drbdadm down r0; rm -f /etc/drbd.d/r0.res")


def e1_live_peer_id_change():
    start_io_loop(n1)
    time.sleep(2)
    write_conf(n1, {"n1": 0, "n2": 7, "n3": 2})
    rc, out = sh(n1, "drbdadm adjust r0")
    time.sleep(8)
    note("E1 adjust with peer n2 id 1->7", f"rc={rc} out={out!r}")
    note("E1 status after adjust", status(n1).replace("\n", " | "))
    write_conf(n1, BASE)
    rc, out = sh(n1, "drbdadm adjust r0")
    note("E1 revert adjust", f"rc={rc} out={out!r}")
    ok, out = wait_for(n1, "peer-disk:UpToDate", 2, 120)
    note("E1 recovers after revert", f"{ok}: {out.replace(chr(10), ' | ')}")
    good, bad = stop_io_loop(n1)
    note("E1 io during churn", f"acked-writes={good} failed-writes={bad}")


def e2_replace_with_new_id(forget_first):
    """n3 dies; n4 joins under id 3 via a live adjust on the survivors."""
    n3.crash()
    NEW = {"n1": 0, "n2": 1, "n4": 3}
    start_io_loop(n1)
    for m in (n1, n2):
        write_conf(m, NEW)
        rc, out = sh(m, "drbdadm adjust r0")
        note(f"E2 adjust on {m.name}", f"rc={rc} out={out!r}")
        if forget_first and rc != 0:
            note(f"E2 forget-peer 2 on {m.name}", str(sh(m, "drbdsetup forget-peer r0 2")))
            note(f"E2 retry adjust on {m.name}", str(sh(m, "drbdadm adjust r0")))
    write_conf(n4, NEW)
    n4.succeed("mkdir -p /var/lib/drbd")
    flag = "--max-peers=7 " if not forget_first else ""
    note("E2 n4 create-md", str(sh(n4, f"drbdadm create-md --force {flag}r0"))[:80])
    note("E2 n4 up", str(sh(n4, "drbdadm up r0")))
    ok, out = wait_for(n1, "peer-disk:UpToDate", 2, 180)
    note("E2 n4 reaches UpToDate", f"{ok}: {out.replace(chr(10), ' | ')}")
    good, bad = stop_io_loop(n1)
    note("E2 io during replacement", f"acked-writes={good} failed-writes={bad}")
    if ok:
        clean, oos = verify_clean(n1, 2)
        note("E2 verify (3 replicas equal)", f"{clean} out-of-sync={oos}")
        note("E2 forget dead peer id 2", str(sh(n1, "drbdsetup forget-peer r0 2")))


def e3_same_id_blank_rebuild():
    n4.succeed("drbdadm down r0")
    n4.succeed("drbdadm create-md --force --max-peers=7 r0")
    n4.succeed("drbdadm up r0")
    started = time.time()
    ok, out = wait_for(n1, "peer-disk:UpToDate", 2, 300)
    note("E3 settle time", f"{time.time() - started:.0f}s")
    note("E3 same-id blank rebuild reaches UpToDate", f"{ok}: {out.replace(chr(10), ' | ')}")
    if ok:
        clean, oos = verify_clean(n1, 2)
        note("E3 verify", f"{clean} out-of-sync={oos}")


def e4_returning_host_high_id():
    n4.crash()
    n3.start()
    n3.wait_for_unit("multi-user.target")
    n3.succeed("modprobe drbd")
    n3.succeed("mkdir -p /var/lib/drbd")
    HIGH = {"n1": 0, "n2": 1, "n3": 20}
    for m in (n1, n2, n3):
        write_conf(m, HIGH)
    for m in (n1, n2):
        rc, out = sh(m, "drbdadm adjust r0")
        note(f"E4 adjust on {m.name}", f"rc={rc} out={out!r}")
    note("E4 n3 create-md", str(sh(n3, "drbdadm create-md --force --max-peers=7 r0"))[:80])
    note("E4 n3 up", str(sh(n3, "drbdadm up r0")))
    ok, out = wait_for(n1, "peer-disk:UpToDate", 2, 180)
    note("E4 host returns as id 20 (was 2), reaches UpToDate", f"{ok}: {out.replace(chr(10), ' | ')}")
    clean, oos = verify_clean(n1, 2)
    note("E4 verify", f"{clean} out-of-sync={oos}")


def e2_forget_before_add():
    """n3 dies; survivors drop it AND forget its slot before n4 is added."""
    n3.crash()
    start_io_loop(n1)
    for m in (n1, n2):
        write_conf(m, {"n1": 0, "n2": 1})
        note(f"E2c drop n3 on {m.name}", str(sh(m, "drbdadm adjust r0")))
        note(f"E2c forget-peer 2 on {m.name}", str(sh(m, "drbdsetup forget-peer r0 2")))
    NEW = {"n1": 0, "n2": 1, "n4": 3}
    for m in (n1, n2):
        write_conf(m, NEW)
        note(f"E2c add n4 on {m.name}", str(sh(m, "drbdadm adjust r0")))
    write_conf(n4, NEW)
    n4.succeed("mkdir -p /var/lib/drbd")
    n4.succeed("drbdadm create-md --force --max-peers=7 r0")
    n4.succeed("drbdadm up r0")
    ok, out = wait_for(n1, "peer-disk:UpToDate", 2, 180)
    note("E2c n4 reaches UpToDate", f"{ok}: {out.replace(chr(10), ' | ')}")
    good, bad = stop_io_loop(n1)
    note("E2c io during replacement", f"acked-writes={good} failed-writes={bad}")
    clean, oos = verify_clean(n1, 2)
    note("E2c verify (3 replicas equal)", f"{clean} out-of-sync={oos}")


def e5_zombie_replica_returns():
    """The forgotten n3 returns with stale data and its old config."""
    n3.start()
    n3.wait_for_unit("multi-user.target")
    n3.succeed("modprobe drbd")
    write_conf(n3, BASE)
    note("E5 zombie up", str(sh(n3, "drbdadm up r0")))
    time.sleep(10)
    note("E5 zombie status on n3", sh(n3, "drbdadm status r0")[1].replace("\n", " | "))
    note("E5 cluster status on n1", status(n1).replace("\n", " | "))
    note("E5 zombie promote attempt", str(sh(n3, "drbdadm primary r0")))


start_all()
for m in MACHINES:
    m.wait_for_unit("multi-user.target")
    m.succeed("modprobe drbd")

version = n1.succeed("cat /sys/module/drbd/version").strip()
print(f"drbd module {version}; utils: {n1.succeed('drbdadm --version | head -3')}")
assert version.startswith("9."), f"need drbd 9.x, got {version}"

round_name = "[default max-peers]"
with subtest("round A: default max-peers"):
    bring_up(None)
    e1_live_peer_id_change()
    e2_replace_with_new_id(forget_first=True)

round_name = "[max-peers=7]"
with subtest("round B: max-peers=7"):
    reset_cluster()
    bring_up(7)
    e1_live_peer_id_change()
    e2_replace_with_new_id(forget_first=False)
    e3_same_id_blank_rebuild()
    e4_returning_host_high_id()

round_name = "[max-peers=7, forget first]"
with subtest("round C: forget the dead peer before adding the replacement"):
    reset_cluster()
    bring_up(7)
    e2_forget_before_add()
    e5_zombie_replica_returns()

print("=" * 72)
for experiment, observed in findings:
    print(f"{experiment}: {observed}")
print("=" * 72)
print("VOL-DRBD-NODEID-SPIKE COMPLETED")
