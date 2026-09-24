"""PHASE-05-TASKS.md Stream D (X5, non-blocking): the vertical slice --
pgbench sustains continuous load and a precisely-tracked ack writer runs
through a hard primary kill (Stream B's X2 mechanism and correctness
oracle), then, on the SAME promoted primary from the SAME run, amcheck
and pg_checksums confirm zero corruption (Stream C's X3) and the old
primary's node rejoins as a genuine streaming replica (Stream C's X4).
Exercising X2-X4 together in one continuous run, rather than as isolated
scenarios, is the point: it is the closest this project gets to a real
operational failover before Phase 8's actual backup/restore work lands.

Runs after cluster-common.py, client-common.py (with `client` bound to
the external VM), block-common.py and python/vol_cluster.py. Expects
VIP_POOL (two addresses, db-postgres-vertical-slice.nix) spliced in
ahead of this file. Helpers below are the union of
db_postgres_failover.py's and db_postgres_recovery.py's own -- kept in
lockstep with both rather than imported, matching every prior VM test's
own convention of a self-contained testScript.
"""

import shlex

PORT = 5432       # VIP-exposed, client-facing port
PG_PORT = 55432   # postgres's own internal listen port, reachable directly
DATABASE = "appdb"
REPL_PASSWORD = "repl-s3cret"
SUPER_PASSWORD = "super-s3cret"
REPLICAS = 3
STATIC_UID = 8332  # internal/blocks/pgha.StaticUID -- pgdata's owning uid
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


def wait_is_replica(node_name, timeout):
    """Poll node_name directly until it reports pg_is_in_recovery()=t --
    proof it has rejoined as a streaming standby, not a diverged primary
    (X4)."""
    deadline = time.time() + timeout
    last = (None, None)
    while time.time() < deadline:
        rc, out = node_psql(IP[node_name], "SELECT pg_is_in_recovery()")
        last = (rc, out)
        if rc == 0 and out == "t":
            return
        time.sleep(2)
    raise AssertionError(f"{node_name} never reported pg_is_in_recovery()=t within {timeout}s (last: {last})")


def wait_replica_count(node_name, want, timeout):
    """Poll node_name's own pg_stat_replication row count -- how this
    test confirms a rejoined standby is actually STREAMING, not merely
    reachable and in recovery (which is also transiently true mid
    pg_basebackup)."""
    deadline = time.time() + timeout
    got = -1
    while time.time() < deadline:
        rc, out = node_psql(IP[node_name], "SELECT count(*) FROM pg_stat_replication")
        if rc == 0:
            try:
                got = int(out)
            except ValueError:
                got = -1
            if got == want:
                return
        time.sleep(2)
    raise AssertionError(f"{node_name}: pg_stat_replication count never reached {want} within {timeout}s (last {got})")


def placement_index(b, node_name):
    """The replica index node_name holds in b's placements, or None."""
    for p in (b.get("status") or {}).get("placements", []):
        if p.get("nodeId") == node_name and p.get("phase") != "LOST" and p.get("replicaIndex", 0) >= 0:
            return p.get("replicaIndex", 0)
    return None


def replica_mount(m, idx):
    """A replica's own real host mount path, resolved via its
    independent per-replica volume (D3) -- db_postgres.py's own
    replica_sockdir resolves the same volume, one level deeper
    (.../mnt/.expanse-postgres/sock)."""
    vname = f"blk-default-pg-pgdata-{idx}"
    row = volume_row(m, vname)
    assert row, f"no volume {vname} visible on {m.name} yet"
    return f"/var/lib/expanse/volumes/{row['id']}/mnt"


def as_pguser(cmd):
    """Runs cmd as pgdata's own owning uid (internal/blocks/pgha.StaticUID)
    rather than the test driver's default root -- postgres's own
    frontend tools (pg_checksums among them) refuse to run as root
    outright, the same restriction that is why this whole block type
    runs its workload under a fixed non-root uid to begin with (Stream A
    X1's own StaticUID doc comment). setpriv takes a bare numeric uid,
    no /etc/passwd entry required."""
    return f"setpriv --reuid={STATIC_UID} --regid={STATIC_UID} --clear-groups -- sh -c {shlex.quote(cmd)}"


def wait_row_replicates(m, sockdir, sql, want, timeout=60):
    """Poll a replica's own local read until streaming replication has
    caught the primary's write, or timeout."""
    deadline = time.time() + timeout
    got = ""
    while time.time() < deadline:
        rc, out = m.execute(
            f"psql -h {sockdir} -p {PG_PORT} -U postgres -d {DATABASE} -v ON_ERROR_STOP=1 -tA -c {shlex.quote(sql)} 2>&1"
        )
        got = out.strip()
        if rc == 0 and got == want:
            return
        time.sleep(1)
    raise AssertionError(f"{m.name}: {sql!r} never returned {want!r} within {timeout}s (last: {got!r})")


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
        # No --exit-on-abort (the default): a client whose connection
        # dies mid-failover just aborts that one client, pgbench itself
        # keeps running the others and returns normally.
        f"  PGPASSWORD={SUPER_PASSWORD} pgbench -h {vip} -p {PORT} -U postgres -d {DATABASE} "
        "-c 4 -j 2 -T 20 >>/root/pgbench.log 2>&1\n"
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


form("pgvs")
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
    primary_idx = placement_index(b, primary_name)
    assert primary_idx is not None, f"no live placement for {primary_name}: {b.get('status')}"
    print(f"current primary: {primary_name} (replica {primary_idx})")

with subtest("both replicas are already streaming before the kill (not still bootstrapping)"):
    # If a survivor hasn't finished its OWN first-ever pg_basebackup
    # when the primary dies, X3/X4's own hasBootstrapped gate correctly
    # sends it straight to a fresh re-clone from the new primary rather
    # than a light retarget (pgha.go's own retargetIfPrimaryMoved) --
    # exactly the right behavior, but until that re-clone finishes,
    # synchronous_standby_names has zero eligible standbys and every
    # commit on the new primary hangs forever (found running this test:
    # pg_stat_activity showed the ack-writer's own INSERT stuck on
    # wait_event=SyncRep while pg_stat_replication showed the survivor
    # connected only as a pg_basebackup client, not a streaming
    # standby). Waiting for both replicas to already be streaming before
    # the kill is what db-postgres-failover.nix's own X2 test gets for
    # free from its simpler setup's extra elapsed time; this test's own
    # heavier --data-checksums-enabled setup needs it made explicit.
    wait_replica_count(primary_name, REPLICAS - 1, timeout=120)

with subtest("create the ack-tracked and corruption-check tables, initialize pgbench"):
    psql_client(VIP, PORT, f"CREATE TABLE {STREAM_TABLE} (id integer primary key, val text)")
    psql_client(VIP, PORT, "CREATE TABLE recovery_check (id serial primary key, val text)")
    psql_client(VIP, PORT, "INSERT INTO recovery_check (val) VALUES ('before-kill')")
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
        print("[diag] ack-writer.log FULL: " + client.execute("cat /root/ack-writer.log")[1])
        print("[diag] pgbench.log tail: " + client.execute("tail -60 /root/pgbench.log")[1])
        print("[diag] pg-ack-writer unit status: " +
              client.execute("systemctl status pg-ack-writer --no-pager 2>&1")[1])
        print("[diag] pg-ack-writer journal: " +
              client.execute("journalctl -u pg-ack-writer --no-pager 2>&1")[1])
        print("[diag] new primary pg_stat_activity: " +
              node_psql(IP[new_primary_name],
                        "SELECT pid, state, wait_event_type, wait_event, query "
                        "FROM pg_stat_activity WHERE datname = 'appdb'")[1])
        print("[diag] new primary pg_stat_replication: " +
              node_psql(IP[new_primary_name],
                        "SELECT application_name, state, sync_state, "
                        "replay_lag FROM pg_stat_replication")[1])
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

with subtest("amcheck reports zero corruption on the promoted primary (X3)"):
    psql_client(VIP, PORT, "CREATE EXTENSION IF NOT EXISTS amcheck")
    # Every btree index in the public schema -- pgbench's own tables
    # (pgbench_branches/tellers/accounts) plus ack_log/recovery_check,
    # all created above. bt_index_check raises on any detected
    # corruption, so psql's own ON_ERROR_STOP makes a non-zero rc the
    # failure signal.
    psql_client(VIP, PORT,
                "SELECT bt_index_check(index => c.oid, heapallindexed => true) "
                "FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid "
                "JOIN pg_am am ON c.relam = am.oid "
                "WHERE am.amname = 'btree' AND c.relnamespace = 'public'::regnamespace")
    print("amcheck: no corruption detected across every public-schema btree index")

with subtest("pg_checksums reports zero corruption on the promoted primary (X3)"):
    new_primary_idx = placement_index(b, new_primary_name)
    assert new_primary_idx is not None, f"no live placement for {new_primary_name}: {b.get('status')}"
    new_primary_m = NODE_BY_NAME[new_primary_name]
    unit = f"expanse-block@default-pg-{new_primary_idx}.service"
    mount = replica_mount(new_primary_m, new_primary_idx)
    pgdata = mount + "/pgdata"
    # setpriv drops to pgdata's own owning uid, but that alone was not
    # enough to read pgdata's own files (db_postgres_recovery.py's own
    # note explains why in full): both the VOLUME directory chain ABOVE
    # pgdata and pgdata's own contents need read/traverse granted.
    # postgres itself refuses to START against a data directory MODE
    # looser than 0700/0750, though -- restored to 0700 right after the
    # check, before the restart below.
    volume_root = mount.removesuffix("/mnt")
    new_primary_m.succeed(f"chmod o+rX {volume_root} {mount} && chmod -R o+rX {pgdata}")
    # A brief, deliberate stop/check/restart of the SAME managed unit,
    # not a lasting disruption -- by this point nothing but this check
    # is still exercising the promoted primary's own write path (the
    # load generators were already stopped above). Stopped by signalling
    # postgres's own PID directly, as root -- db_postgres_recovery.py's
    # own note explains why `pg_ctl stop` under setpriv and `systemctl
    # stop` both failed here. SIGINT is a postmaster fast shutdown, the
    # same signal pg_ctl -m fast sends.
    pg_pid = new_primary_m.succeed(f"head -1 {pgdata}/postmaster.pid").strip()
    new_primary_m.succeed(f"kill -INT {pg_pid}")
    new_primary_m.succeed(f"timeout 30 sh -c 'while kill -0 {pg_pid} 2>/dev/null; do sleep 0.5; done'")
    out = new_primary_m.succeed(as_pguser(f"pg_checksums -D {pgdata} -c"))
    print(f"[diag] pg_checksums: {out}")
    # Restore pgdata's own strict mode -- postgres's restart below
    # refuses to start otherwise.
    new_primary_m.succeed(f"chmod 700 {pgdata}")
    new_primary_m.succeed(f"systemctl start {unit}")
    find_primary([new_primary_name], timeout=60)
    print("pg_checksums: zero bad checksums on the promoted primary")

with subtest("the old primary's node rejoins as a streaming replica, not diverged (X4)"):
    NODE_BY_NAME[primary_name].start()
    wait_agent_ready(NODE_BY_NAME[primary_name])
    # Generous budget: the rejoined node must reboot, its agent must run
    # a pass, discover pgha's own demotion (another node already won the
    # lease for real), stop and wipe its diverged PGDATA, get restarted
    # by systemd's Restart=on-failure, and re-clone via pg_basebackup --
    # several real, sequenced steps, not a single fast state flip.
    wait_is_replica(primary_name, timeout=240)
    wait_replica_count(new_primary_name, REPLICAS - 1, timeout=120)
    print(f"{primary_name} rejoined as a streaming replica of {new_primary_name}, no manual pg_rewind")

with subtest("the rejoined replica actually streams new writes, not just a stale snapshot"):
    psql_client(VIP, PORT, "INSERT INTO recovery_check (val) VALUES ('after-rejoin')")
    sockdir = replica_mount(NODE_BY_NAME[primary_name], primary_idx) + "/.expanse-postgres/sock"
    wait_row_replicates(NODE_BY_NAME[primary_name], sockdir,
                         "SELECT val FROM recovery_check WHERE val = 'after-rejoin'", "after-rejoin")

print("DB-POSTGRES-VERTICAL-SLICE DONE")
