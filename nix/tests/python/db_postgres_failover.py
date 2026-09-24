"""PHASE-05-TASKS.md Stream B (X2, the phase's decider): a db/postgres
block survives losing the node running its current primary. pgbench
sustains continuous load, and a second, precisely-tracked writer records
exactly which transactions the client believed committed. The primary's
node is hard-killed; a surviving replica promotes itself automatically
via pgha's lease-loss path (D1/D4), the client's connection to the same
stable VIP endpoint resumes without a manual reconnect, and every
acknowledged write is still present on the promoted primary -- no
acknowledged transaction lost (D5's split-brain guard plus
synchronous_standby_names, or this test is what finds out otherwise).

Runs after cluster-common.py, client-common.py (with `client` bound to
the external VM), block-common.py and python/vol_cluster.py. Expects
VIP_POOL (two addresses, db-postgres-failover.nix) spliced in ahead of
this file.
"""

import shlex

PORT = 5432       # VIP-exposed, client-facing port
PG_PORT = 55432   # postgres's own internal listen port, reachable directly
DATABASE = "appdb"
REPL_PASSWORD = "repl-s3cret"
SUPER_PASSWORD = "super-s3cret"
REPLICAS = 3
STREAM_TABLE = "ack_log"
MIN_ACKS_BEFORE_KILL = 5
MIN_ACKS_AFTER_RECOVERY = 5

MANIFEST = f"""apiVersion: expanse.io/v1
kind: Block
metadata:
  name: pg
  namespace: default
spec:
  type: db/postgres
  replicas: {REPLICAS}
  placement:
    antiAffinity: ANTI_AFFINITY_NODE
  resources:
    requests:
      cpu: 200m
      memory: 256Mi
  storage:
    - name: pgdata
      size: 256Mi
      replication: 1
      mountPath: /var/lib/postgresql-data
  config:
    port: {PG_PORT}
    database: {DATABASE}
    replicationPassword: {REPL_PASSWORD}
    superuserPassword: {SUPER_PASSWORD}
    sharedBuffers: 32MB
    maxWalSenders: 10
    maxReplicationSlots: 10
  network:
    ports:
      - name: pg
        port: {PORT}
        target_port: {PG_PORT}
        protocol: tcp
        expose: EXPOSE_VIP
    health_check:
      readiness:
        type: PROBE_TCP
        port: {PG_PORT}
        period_seconds: 2
"""

NODE_BY_NAME = {"n1": n1, "n2": n2, "n3": n3}


def vip_holders(vip_addr):
    """Nodes currently carrying vip_addr on eth1."""
    holders = []
    for m in [n1, n2, n3]:
        rc, out = m.execute(f"ip -4 -o addr show eth1 | grep -F {vip_addr} || true")
        if rc == 0 and out.strip():
            holders.append(m.name)
    return holders


def wait_single_holder(vip_addr, timeout, want_nodes=None):
    deadline = time.time() + timeout
    holders = []
    while time.time() < deadline:
        holders = vip_holders(vip_addr)
        if len(holders) == 1 and (want_nodes is None or holders[0] in want_nodes):
            return holders[0]
        time.sleep(2)
    raise AssertionError(f"{vip_addr}: never settled on exactly 1 holder in {want_nodes} (last: {holders})")


def psql_client(host, port, sql, timeout=240):
    """Run one SQL statement from the external client, over the VIP.
    Retries: db-postgres.py's own note applies unchanged here -- the VIP
    can still be settling, or a replica finishing its own bootstrap,
    even after wait_single_holder first sees it land."""
    cmd = (f"PGPASSWORD={SUPER_PASSWORD} psql -h {host} -p {port} -U postgres -d {DATABASE} "
           f"-v ON_ERROR_STOP=1 -tA -c {shlex.quote(sql)}")
    deadline = time.time() + timeout
    rc, out = 1, ""
    while time.time() < deadline:
        rc, out = client.execute(cmd)
        if rc == 0:
            return out.strip()
        time.sleep(2)
    raise AssertionError(f"psql via VIP never succeeded within {timeout}s (last rc={rc}): {out}")


def node_psql(node_ip, sql, timeout=10):
    """One SQL statement direct to a node's own PG_PORT, bypassing the
    VIP/LB entirely -- how this test tells WHICH node is primary, not
    just that a write connection through the VIP reached one."""
    cmd = (f"PGPASSWORD={SUPER_PASSWORD} timeout {timeout} psql -h {node_ip} -p {PG_PORT} -U postgres -d {DATABASE} "
           f"-v ON_ERROR_STOP=1 -tA -c {shlex.quote(sql)}")
    rc, out = client.execute(cmd)
    return rc, out.strip()


def find_primary(node_names, timeout):
    """Poll node_names directly until exactly one reports
    pg_is_in_recovery()=f. Never touches a crashed node -- callers pass
    only live survivors once one has been crash()ed (share_smb_failover.py's
    vip_holders docstring explains why in full: querying a crashed
    machine object silently reboots it)."""
    deadline = time.time() + timeout
    last = {}
    while time.time() < deadline:
        primaries = []
        last = {}
        for name in node_names:
            rc, out = node_psql(IP[name], "SELECT pg_is_in_recovery()")
            last[name] = (rc, out)
            if rc == 0 and out == "f":
                primaries.append(name)
        if len(primaries) == 1:
            return primaries[0]
        time.sleep(2)
    raise AssertionError(f"never settled on exactly one primary among {node_names} within {timeout}s: {last}")


def start_ack_writer(vip):
    """A background loop on the client, inserting one uniquely-numbered
    row per iteration through the VIP. Each insert is its own
    connection/transaction (psql -c), so a row lost to a broken
    connection mid-failover is simply retried as the NEXT number, never
    blocking forever on one lost round-trip -- last_acked is the cheap
    poll target, acked_ids the precise log the final correctness check
    reads. Directly parallel to share_smb_failover.py's writer.sh."""
    client.succeed("rm -f /root/last_acked /root/acked_ids /root/ack-writer.log")
    client.succeed(
        "cat > /root/ack-writer.sh << 'EOF'\n"
        "#!/bin/sh\n"
        # systemd-run's default $PATH is minimal on NixOS -- without
        # this, "sleep"/"timeout" resolve to nothing and the loop
        # free-spins or fails outright (share_smb_failover.py's own note).
        "export PATH=/run/current-system/sw/bin:$PATH\n"
        "i=0\n"
        "while true; do\n"
        "  i=$((i+1))\n"
        f"  if PGPASSWORD={SUPER_PASSWORD} psql -h {vip} -p {PORT} -U postgres -d {DATABASE} "
        f"-v ON_ERROR_STOP=1 -tAc \"INSERT INTO {STREAM_TABLE}(id, val) VALUES ($i, 'seq-' || $i)\" "
        "1>>/root/ack-writer.log 2>&1; then\n"
        "    echo \"$i\" > /root/last_acked\n"
        "    echo \"$i\" >> /root/acked_ids\n"
        "  fi\n"
        "  sleep 0.3\n"
        "done\n"
        "EOF\n"
    )
    client.succeed("systemd-run --unit=pg-ack-writer /bin/sh /root/ack-writer.sh")


def start_pgbench_load(vip):
    """pgbench sustains continuous load (X2's own wording) through the
    whole failover window -- real connection churn and write pressure
    alongside the precisely-tracked ack writer above, which is the
    correctness oracle. Looped in short (-T) bursts rather than one
    unbounded run so a burst that dies mid-failover (its own connection
    caught in the same outage as the ack writer's) is simply restarted,
    not left as a single stalled process."""
    client.succeed("rm -f /root/pgbench.log")
    client.succeed(
        "cat > /root/pgbench-loop.sh << 'EOF'\n"
        "#!/bin/sh\n"
        "export PATH=/run/current-system/sw/bin:$PATH\n"
        "while true; do\n"
        f"  PGPASSWORD={SUPER_PASSWORD} pgbench -h {vip} -p {PORT} -U postgres -d {DATABASE} "
        "-c 4 -j 2 -T 20 --continue-on-error >>/root/pgbench.log 2>&1\n"
        "done\n"
        "EOF\n"
    )
    client.succeed("systemd-run --unit=pg-bench-load /bin/sh /root/pgbench-loop.sh")


def last_acked():
    rc, out = client.execute("cat /root/last_acked 2>/dev/null || echo 0")
    try:
        return int(out.strip())
    except ValueError:
        return 0


def wait_acked_at_least(n, timeout, what):
    deadline = time.time() + timeout
    got = last_acked()
    while time.time() < deadline:
        got = last_acked()
        if got >= n:
            return got
        time.sleep(1)
    raise AssertionError(f"{what}: only {got} acked writes within {timeout}s (want >= {n})")


form("pgfo")
wait_agent_ready(n1)
wait_agent_ready(n2)
wait_agent_ready(n3)

with subtest("the cluster's own management UI claims one pool address"):
    deadline = time.time() + 60
    ui_vip = None
    while time.time() < deadline and ui_vip is None:
        for pool_addr in VIP_POOL:
            if len(vip_holders(pool_addr)) == 1:
                ui_vip = pool_addr
                break
        if ui_vip is None:
            time.sleep(2)
    assert ui_vip, f"no VIP claimed within 60 s (expected the UI to take one): {VIP_POOL}"

with subtest("deploy a 3-replica db/postgres block, each replica its own volume"):
    deploy(n1, "pg", MANIFEST)
    b = wait_phase(n1, "pg", ["RUNNING"], 300)
    nodes = sorted(placement_nodes(b))
    assert len(nodes) == REPLICAS, f"pg placed on {nodes}, want {REPLICAS} distinct nodes: {b.get('status')}"

with subtest("the block claims the other pool address as its own VIP"):
    remaining = [a for a in VIP_POOL if a != ui_vip]
    assert len(remaining) == 1, f"VIP_POOL must have exactly 2 addresses: {VIP_POOL}"
    VIP = remaining[0]
    holder = wait_single_holder(VIP, 60, nodes)
    assert holder, f"pg's VIP ({VIP}) never settled on one of its own replicas {nodes}"

with subtest("identify the current primary directly, not just the VIP holder"):
    # The VIP holder (net-vip's own lease) and the postgres primary
    # (pgha's separate lease) are independent mechanisms -- they need
    # not be the same node, so the node to kill is found by asking each
    # replica's own postgres, not by re-using `holder`.
    primary_name = find_primary(nodes, timeout=120)
    print(f"current primary: {primary_name}")

with subtest("create the ack-tracked table and initialize pgbench, both through the VIP"):
    psql_client(VIP, PORT, f"CREATE TABLE {STREAM_TABLE} (id integer primary key, val text)")
    client.succeed(
        f"PGPASSWORD={SUPER_PASSWORD} pgbench -h {VIP} -p {PORT} -U postgres -i --scale=1 {DATABASE}"
    )

with subtest("pgbench sustains load while a precisely-tracked writer runs"):
    start_pgbench_load(VIP)
    start_ack_writer(VIP)
    wait_acked_at_least(MIN_ACKS_BEFORE_KILL, 60, "pre-kill warmup")

with subtest("kill the primary's whole VM mid-write"):
    acked_at_kill = last_acked()
    t0 = time.time()
    NODE_BY_NAME[primary_name].crash()

with subtest("a surviving replica promotes itself automatically (X2)"):
    survivor_names = [n for n in nodes if n != primary_name]
    new_primary_name = find_primary(survivor_names, timeout=90)
    reconverge_s = time.time() - t0
    print(f"new primary {new_primary_name} elected after {reconverge_s:.1f}s")

with subtest("the client's writer resumes against the same VIP without a manual reconnect (X2)"):
    try:
        resumed_at = wait_acked_at_least(acked_at_kill + MIN_ACKS_AFTER_RECOVERY, 180,
                                          "post-failover writes to resume")
    except AssertionError:
        print("[diag] ack-writer.log tail: " + client.execute("tail -40 /root/ack-writer.log")[1])
        print("[diag] pgbench.log tail: " + client.execute("tail -40 /root/pgbench.log")[1])
        for name in survivor_names:
            m = NODE_BY_NAME[name]
            print(f"[diag] {name} pgha journal: " +
                  m.execute("journalctl -u expansed --no-pager -n 200 | grep -i pgha 2>&1")[1])
        raise
    print(f"writer resumed: {resumed_at} acked (was {acked_at_kill} at kill)")

with subtest("stop the load generators"):
    client.succeed("systemctl stop pg-ack-writer 2>/dev/null || true")
    client.succeed("systemctl stop pg-bench-load 2>/dev/null || true")

with subtest("every acknowledged write survived the failover -- no acked transaction lost (X2)"):
    acked_ids_raw = client.succeed("cat /root/acked_ids").strip()
    acked_ids = {int(x) for x in acked_ids_raw.splitlines() if x.strip()}
    assert acked_ids, "no writes were ever acknowledged -- test setup bug, not a real pass"
    db_ids_raw = psql_client(VIP, PORT, f"SELECT string_agg(id::text, ',') FROM {STREAM_TABLE}")
    db_ids = {int(x) for x in db_ids_raw.split(",") if x.strip()}
    missing = acked_ids - db_ids
    assert not missing, (
        f"{len(missing)} of {len(acked_ids)} acknowledged transactions lost after failover: "
        f"missing ids (sample) {sorted(missing)[:20]}"
    )
    print(f"verified all {len(acked_ids)} acknowledged writes survived failover onto {new_primary_name}")

print("DB-POSTGRES-FAILOVER DONE")
