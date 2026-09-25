"""chaos-soak (Phase 11 X2, the release blocker): storage, cluster and network faults
injected concurrently over an extended soak, with zero acked-write loss checked
continuously against ARCHITECTURE.md §8's own unconditional bar.

Storage and cluster faults each take a node fully down (hard-crash the volume's primary,
or stop/partition a non-leader) and so share ONE lock: with 3 nodes and quorum 2, this
project's raft tolerates exactly one node down at a time, not two -- running both pools
unguarded could pick two different victims and stall the whole cluster, which is not the
scenario X2 exists to prove (PHASE-11-TASKS.md D2). Each round picks one of the two at
random and runs its full inject-verify-restore cycle before the next round is scheduled.

The network pool injects LATENCY (tc netem delay), never a drop: delay never removes a
node from the mesh, so it is safe to run concurrently with the storage/cluster pool's
node-down fault. It is backgrounded on the guest with its own self-clearing timer (the
same nohup idiom net-vip-failover.nix already uses for its load generator), so the host
driver never blocks waiting on it -- this is what actually makes "more than one fault in
flight at once" true, per D2, without real host-side threading.

Reuses vol_durability_rec.py's ledger/writer/verify idiom unchanged for the zero-acked-
loss check: X2 restates §8's bar under compound fault load, it does not redefine "acked".

Runs after cluster-common.py and vol_cluster.py. EXPANSE_CHAOS_SOAK_SECONDS overrides the
default hour-long soak, mirroring EXPANSE_DURABILITY_ITERS's precedent in vol-durability.nix.
"""

import os
import random

SOAK_SECONDS = int(os.environ.get("EXPANSE_CHAOS_SOAK_SECONDS", "3600"))
SIZE_MIB = 256
LEDGER_PORT = 9440
MIN_ACKED_BEFORE_KILL = 20
WRITER_PAUSE_MS = 2
VG = "vg0"
NET_FAULT_DURATION_S = (10, 30)
NET_FAULT_GAP_S = (5, 20)
NODE_FAULT_GAP_S = (15, 40)

rng = random.Random(0xC4A05)  # deterministic, so a failing run reproduces


def rec(m, args):
    return m.execute(f"{REC} {args}")


def ledger_highest(host, path):
    return int(rec(host, f"highest {path}")[1].strip())


def start_ledger(host, path):
    host.execute("systemctl stop dur-ledger 2>/dev/null")
    host.succeed(f"systemd-run --unit=dur-ledger {REC} ledger {LEDGER_PORT} {path}")
    host.wait_until_succeeds(f"ss -ltn | grep -q :{LEDGER_PORT}", timeout=30)


def stream_until_killed(primary, host, path, start):
    """Write from record `start` until enough are acked, then hard-kill the primary.
    Identical to vol_durability_main.py's helper of the same name -- reused, not
    reinvented, per D2."""
    dev = device_of(primary)
    primary.succeed(
        f"systemd-run --unit=dur-writer {REC} write {dev} {addr(host)} {LEDGER_PORT} {start} {WRITER_PAUSE_MS}"
    )
    wait_for(lambda: ledger_highest(host, path) >= start + MIN_ACKED_BEFORE_KILL, "records to ack", 120)
    time.sleep(rng.uniform(0, 1.0))
    primary.crash()


def assert_no_loss(m, dev, acked, what):
    rc, out = rec(m, f"verify {dev} {acked}")
    assert rc == 0, f"ACKED WRITE LOST on {what} ({acked} acked): {out} (release blocker, X2)"


def wait_single_primary(nodes, timeout=300):
    wait_for(lambda: len(primaries(res, nodes)) == 1, "one primary", timeout)
    return primaries(res, nodes)[0]


def restore(dead):
    dead.start()
    dead.wait_for_unit("multi-user.target", timeout=180)
    dead.wait_for_unit("expansed.service", timeout=120)
    wait_agent_ready(dead)
    wait_for(lambda: all(fully_replicated(m, res) for m in NODES), "every replica UpToDate", 300)


def verify_all_replicas(acked, what):
    for m in NODES:
        assert_no_loss(m, f"/dev/{VG}/{res}", acked, f"replica {m.name} {what}")


def inject_storage_fault(acked):
    """Hard-crash the volume's primary; the survivors must take over with zero acked-
    write loss; restore the old primary and confirm every replica re-converges and
    agrees. Directly the vol-durability.nix iteration, run inside the combined soak."""
    primary = wait_single_primary(NODES)
    host = [m for m in NODES if m is not primary][0]
    survivors = [m for m in NODES if m is not primary]
    ledger = f"/root/ledger-storage-{int(time.time())}"

    start_ledger(host, ledger)
    stream_until_killed(primary, host, ledger, acked)
    new_primary = wait_single_primary(survivors)
    acked = max(acked, ledger_highest(host, ledger) + 1)

    assert_no_loss(new_primary, device_of(new_primary), acked, f"new primary {new_primary.name}")
    restore(primary)
    wait_single_primary(NODES)
    verify_all_replicas(acked, "post-storage-fault")
    print(f"[storage] {primary.name} crashed, {new_primary.name} took over, {acked} acked, replicas agree")
    return acked


def inject_cluster_fault():
    """Partition or hard-stop a non-leader node; the other two keep serving; heal and
    confirm the victim rejoins with zero divergence. Reuses cluster-partition.nix's
    nftables idiom and cluster-node-loss.nix's stop/start idiom, picked at random."""
    leader = leader_of(status(n1))
    victim = rng.choice([m for m in NODES if m.name != leader])
    survivors = [m for m in NODES if m is not victim]
    mode = rng.choice(["partition", "kill"])
    canary = f"/chaos/cluster-{int(time.time())}"

    if mode == "kill":
        victim.succeed("systemctl stop expansed.service")
    else:
        victim.succeed(
            "nft add table ip exppart; "
            "nft add chain ip exppart output '{ type filter hook output priority 0; }'; "
            "nft add chain ip exppart input '{ type filter hook input priority 0; }'"
        )
        for m in survivors:
            victim.succeed(f"nft add rule ip exppart output ip daddr {addr(m)} drop")
            victim.succeed(f"nft add rule ip exppart input ip saddr {addr(m)} drop")

    deadline = time.time() + 20
    ok = False
    while time.time() < deadline:
        if any(l in [m.name for m in survivors] for l in leaders(status(survivors[0]))):
            ok = True
            break
        time.sleep(1)
    assert ok, f"survivors leaderless after {mode} of {victim.name}"
    rc, out = kv(survivors[0], f"put {canary} during-fault")
    assert rc == 0, f"write failed with {victim.name} {mode}'d: {out}"

    if mode == "kill":
        victim.succeed("systemctl start expansed.service")
        victim.wait_for_unit("expansed.service")
    else:
        victim.execute("nft delete table ip exppart 2>/dev/null || true")
    wait_quorum("3/2", 45)

    deadline = time.time() + 30
    got = ""
    while time.time() < deadline:
        rc, got = kv(victim, f"get {canary}")
        if rc == 0 and got.strip() == "during-fault":
            break
        time.sleep(1)
    assert got.strip() == "during-fault", f"{victim.name} did not catch up after {mode}: {got!r}"
    print(f"[cluster] {mode} of {victim.name}, survivors kept serving, rejoined clean")


def inject_network_fault():
    """Add tc netem delay to one node's mesh interface for a bounded, self-clearing
    window -- never a drop, so it can never combine with a node-down fault to sever
    quorum (see module docstring)."""
    victim = rng.choice(NODES)
    delay_ms = rng.randint(100, 400)
    dur = rng.randint(*NET_FAULT_DURATION_S)
    victim.succeed(f"tc qdisc replace dev eth1 root netem delay {delay_ms}ms {delay_ms // 4}ms")
    victim.execute(
        f"nohup bash -c 'sleep {dur}; tc qdisc del dev eth1 root netem' > /dev/null 2>&1 &",
        check_return=False,
    )
    print(f"[network] {delay_ms}ms delay on {victim.name} for {dur}s")


form("chaos")

with subtest("volume created and replicated everywhere"):
    n1.succeed(f"expanse ctl volume create chaos --size {SIZE_MIB}Mi --replication 3")
    for m in NODES:
        m.wait_until_succeeds("drbdadm status | grep -q '^vol-'", timeout=180)
    res = n1.succeed("drbdadm status | head -1 | cut -d' ' -f1").strip()
    wait_for(lambda: all(fully_replicated(m, res) for m in NODES), "every replica UpToDate", 300)
    wait_single_primary(NODES)

deadline = time.time() + SOAK_SECONDS
acked = 0
round_n = 0
next_node_fault = time.time() + rng.uniform(*NODE_FAULT_GAP_S)
next_net_fault = time.time() + rng.uniform(*NET_FAULT_GAP_S)

with subtest(f"combined storage+cluster+network chaos soak, {SOAK_SECONDS}s"):
    while time.time() < deadline:
        now = time.time()
        if now >= next_net_fault:
            inject_network_fault()
            next_net_fault = now + rng.uniform(*NET_FAULT_GAP_S)
        if now >= next_node_fault:
            round_n += 1
            with subtest(f"round {round_n} ({acked} acked so far)"):
                if rng.random() < 0.5:
                    acked = inject_storage_fault(acked)
                else:
                    inject_cluster_fault()
            next_node_fault = time.time() + rng.uniform(*NODE_FAULT_GAP_S)
        time.sleep(1)

with subtest("final state: every replica holds every acked record and agrees"):
    final = wait_single_primary(NODES)
    assert_no_loss(final, device_of(final), acked, f"final primary {final.name}")
    verify_all_replicas(acked, "final")
    print(
        f"RELEASE GATE PASSED (X2): {acked} acked records, {round_n} node-fault rounds, survived a "
        f"{SOAK_SECONDS}s combined storage+cluster+network chaos soak, zero acked-write loss"
    )
