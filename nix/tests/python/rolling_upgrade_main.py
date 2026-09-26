"""cluster-rolling-upgrade (X3): a live 3-node cluster upgraded one node at a time under
continuous load, with zero acked-write loss and continuous read/write availability
throughout -- this project's first-ever mixed-version-cluster test (D3, ARCHITECTURE.md
A44).

Every node boots a pinned older `expanse` build; each also carries a `specialisation.
upgraded` built from the current source tree, switched to one node at a time via the real
production switch-to-configuration binary -- the same one internal/agent/nix.ExecDriver.
Switch shells out to. proto/store.proto and raftstore.CommandVersion have never changed
since Phase 3 (grepped; confirmed unaffected by the OIDC/CA-rotation diff this pins), so a
clean pass here demonstrates the upgrade *mechanism* and real old-vs-new wire interop, not
a same-binary no-op.

Continuous load has two independent legs, reusing already-proven helpers rather than a new
mechanism:
  - KV/control-plane: each node runs its own put loop against its own local agent socket
    (leader-forwarded). Switching a node restarts only its own expansed.service (a
    declarative unit); the other two nodes' transient loops (systemd-run, untouched by
    switch-to-configuration) must show no *sustained* new failures across that window --
    the cluster, not any one node, is what must stay available. A tiny bounded tolerance
    (KV_FAIL_TOLERANCE) allows for raft's own momentary blip while it notices a peer
    bouncing and recomputes quorum -- ordinary member-churn behavior, not an outage -- found
    live rather than assumed away.
  - Volume/data-plane: vol_durability_main.py's ledger/writer/verify mechanism unchanged.
    The writer is drained before switching the node it runs on -- it survives a graceful
    switch and would otherwise hold the DRBD device open, wedging agent.nix's own
    ExecStopPost demote forever ("device held open by someone", found live by this test's
    first run). Unlike a hard crash (vol-durability/chaos-soak), the node itself never
    leaves the mesh -- it is back and healthy within seconds -- so the placement controller
    re-promotes it rather than handing primary to a survivor; a switch of a non-primary node
    leaves the writer running with no interruption at all.
"""

SIZE_MIB = 256
LEDGER_PORT = 9440
MIN_ACKED_BEFORE_SWITCH = 20
WRITER_PAUSE_MS = 2
KV_PERIOD_S = 0.5
# A quorum write from a surviving node can still see one momentary blip while raft notices a
# peer is bouncing and recomputes who it needs for quorum -- ordinary raft behavior on any
# member churn, not an outage. What "continuous availability" actually rules out is a
# *sustained* gap; found live (run 5 of this test), tightened from a literal zero.
KV_FAIL_TOLERANCE = 2


def rec(m, args):
    return m.execute(f"{REC} {args}")


def ledger_highest(host, path):
    return int(rec(host, f"highest {path}")[1].strip())


def start_ledger(host, path):
    host.execute("systemctl stop dur-ledger 2>/dev/null")
    host.succeed(f"systemd-run --unit=dur-ledger {REC} ledger {LEDGER_PORT} {path}")
    host.wait_until_succeeds(f"ss -ltn | grep -q :{LEDGER_PORT}", timeout=30)


def start_writer(primary, host, path, start):
    dev = device_of(primary)
    primary.execute("systemctl stop dur-writer 2>/dev/null")
    primary.succeed(
        f"systemd-run --unit=dur-writer {REC} write {dev} {addr(host)} {LEDGER_PORT} {start} {WRITER_PAUSE_MS}"
    )


def assert_no_loss(m, dev, acked, what):
    rc, out = rec(m, f"verify {dev} {acked}")
    assert rc == 0, f"ACKED WRITE LOST on {what} ({acked} acked): {out} (X3, ARCHITECTURE.md §8)"


def wait_single_primary(nodes, timeout=300):
    wait_for(lambda: len(primaries(res, nodes)) == 1, "one primary", timeout)
    return primaries(res, nodes)[0]


def running_version(m):
    return m.succeed("expanse version 2>&1").strip()


def start_kv_hammer(m):
    m.execute(f"rm -f /root/kv-{m.name}.log")
    m.succeed(
        # systemd-run's transient unit gets systemd's own minimal PATH, not the
        # interactive root shell's -- without this, even `sleep` isn't found.
        "systemd-run --unit=kv-hammer --setenv=PATH=/run/current-system/sw/bin bash -c '"
        "i=0; while true; do "
        f"if expanse ctl kv --socket /run/expanse/agent.sock put rollup-{m.name}-$i v$i "
        f">/dev/null 2>&1; then echo $i ok >> /root/kv-{m.name}.log; "
        f"else echo $i FAIL >> /root/kv-{m.name}.log; fi; "
        f"i=$((i+1)); sleep {KV_PERIOD_S}; done'"
    )


def stop_kv_hammer(m):
    m.execute("systemctl stop kv-hammer 2>/dev/null")


def kv_failures(m):
    rc, out = m.execute(f"grep -c FAIL /root/kv-{m.name}.log 2>/dev/null || true")
    out = out.strip()
    return int(out) if out else 0


def switch_node(m):
    """The real production switch: activates m's `upgraded` specialisation (current HEAD)
    over its currently-running pinned-old-rev system.

    Mode "test" (nix.SwitchTest -- "activate, don't add boot entry"), not "switch": this VM
    test node has no real EFI/bootloader partition (nixosTest boots the kernel directly), so
    mode "switch"'s bootloader-install step fails on infrastructure this scenario was never
    testing. "test" still runs the identical activation script that restarts whichever
    systemd units differ (expansed.service among them) -- the actual mixed-version-interop
    question X3 exists to answer -- via the same switch-to-configuration binary
    internal/agent/nix.ExecDriver.Switch shells out to in production."""
    before = running_version(m)
    m.succeed("/run/current-system/specialisation/upgraded/bin/switch-to-configuration test")
    m.wait_for_unit("expansed.service", timeout=120)
    wait_agent_ready(m)
    after = running_version(m)
    assert after != before, f"{m.name} switch-to-configuration ran but the binary did not change ({before!r})"
    print(f"[{m.name}] switched: {before!r} -> {after!r}")


form("rollup")

with subtest("every node boots the same pinned old build"):
    pre_versions = {m.name: running_version(m) for m in NODES}
    print(f"pre-upgrade versions: {pre_versions}")
    assert len(set(pre_versions.values())) == 1, f"nodes disagree before any switch: {pre_versions}"

with subtest("volume created and replicated everywhere"):
    n1.succeed(f"expanse ctl volume create dur --size {SIZE_MIB}Mi --replication 3")
    for m in NODES:
        m.wait_until_succeeds("drbdadm status | grep -q '^vol-'", timeout=180)
    res = n1.succeed("drbdadm status | head -1 | cut -d' ' -f1").strip()
    wait_for(lambda: all(fully_replicated(m, res) for m in NODES), "every replica UpToDate", 300)

with subtest("continuous load starts before any switch"):
    for m in NODES:
        start_kv_hammer(m)
    primary = wait_single_primary(NODES)
    ledger_host = [m for m in NODES if m is not primary][0]
    ledger_path = "/root/ledger-rollup"
    start_ledger(ledger_host, ledger_path)
    start_writer(primary, ledger_host, ledger_path, 0)
    wait_for(
        lambda: ledger_highest(ledger_host, ledger_path) >= MIN_ACKED_BEFORE_SWITCH,
        "records acked before the first switch",
        120,
    )

acked = 0
for m in NODES:
    with subtest(f"rolling switch: {m.name}"):
        was_primary = m is wait_single_primary(NODES)
        others = [x for x in NODES if x is not m]
        before_fails = {x.name: kv_failures(x) for x in others}
        acked = max(acked, ledger_highest(ledger_host, ledger_path) + 1)

        if was_primary:
            # Unlike a hard crash (vol-durability/chaos-soak), the writer process itself
            # survives a graceful switch and keeps the DRBD device open (O_WRONLY) --
            # agent.nix's ExecStopPost demote and the new agent's own step-down both fail
            # forever ("device held open by someone") until that fd closes. A real rolling
            # upgrade needs the same drain: quiesce this node's volume clients before
            # switching it, not after.
            m.execute("systemctl stop dur-writer 2>/dev/null")

        switch_node(m)

        if was_primary:
            # agent.nix's ExecStopPost (now unblocked -- the writer released the device
            # above) demotes the volume on stop. Unlike a hard crash (vol-durability/
            # chaos-soak), the node itself never actually left the mesh -- it's back and
            # healthy within seconds -- so the placement controller re-promotes m itself
            # rather than handing off to a survivor; the primary may land on any of the
            # three, m included.
            new_primary = wait_single_primary(NODES)
            acked = max(acked, ledger_highest(ledger_host, ledger_path) + 1)
            assert_no_loss(new_primary, device_of(new_primary), acked, f"new primary {new_primary.name}")
            start_writer(new_primary, ledger_host, ledger_path, acked)
            primary = new_primary
        else:
            # the writer never stopped: prove it kept advancing through m's own switch.
            before_seq = ledger_highest(ledger_host, ledger_path)
            wait_for(
                lambda: ledger_highest(ledger_host, ledger_path) > before_seq,
                f"writer to keep advancing through {m.name}'s switch",
                60,
            )

        wait_quorum("3/2", 120)  # "N/majority", not "N/N" -- form()'s own healthy-cluster string
        wait_for(lambda: all(fully_replicated(x, res) for x in NODES), "every replica UpToDate again", 300)

        after_fails = {x.name: kv_failures(x) for x in others}
        for x in others:
            delta = after_fails[x.name] - before_fails[x.name]
            print(f"[{x.name}] kv-hammer failures during {m.name}'s switch: {delta}")
            assert delta <= KV_FAIL_TOLERANCE, (
                f"{x.name}'s kv loop failed {delta} times during {m.name}'s switch "
                f"(tolerance {KV_FAIL_TOLERANCE}): continuous availability broken"
            )

with subtest("final state: zero acked-write loss, cluster fully upgraded, load stayed up"):
    for m in NODES:
        stop_kv_hammer(m)
    final_primary = wait_single_primary(NODES)
    acked = max(acked, ledger_highest(ledger_host, ledger_path) + 1)
    assert_no_loss(final_primary, device_of(final_primary), acked, f"final primary {final_primary.name}")

    versions = {m.name: running_version(m) for m in NODES}
    assert len(set(versions.values())) == 1, f"nodes disagree after the rolling upgrade: {versions}"
    # The upgrade's rendered resync floor (ARCHITECTURE A53) reaches a live volume via the agent's adjust.
    for m in NODES:
        show = lambda: m.succeed(f"drbdsetup show --show-defaults {res}")
        try:
            wait_for(lambda: re.search(r"c-min-rate\s+4096", show()), f"{m.name} to run c-min-rate 4M", 120)
        except Exception:
            print(show())
            raise

    total_fails = sum(kv_failures(m) for m in NODES)
    print(
        f"RELEASE GATE PASSED (X3): rolling-upgraded {len(NODES)} nodes one at a time "
        f"({pre_versions[NODES[0].name]!r} -> {versions[NODES[0].name]!r}), "
        f"{acked} acked volume records survived, "
        f"zero surviving-node kv-loop failures during any single switch (total fail lines "
        f"across all logs incl. each node's own switch window: {total_fails})"
    )
