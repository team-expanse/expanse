"""PHASE-05-TASKS.md Stream C (X3, X4): after a db/postgres primary is
lost and a replica promotes (Stream B's own mechanism), the promoted
primary must be provably uncorrupted (amcheck + pg_checksums, X3), and
the old primary's node, once it rejoins, must come back as a fresh
streaming replica of the new primary -- not a permanently diverged
standalone primary, and not requiring a manual pg_rewind (X4).

Runs after cluster-common.py, client-common.py (with `client` bound to
the external VM), block-common.py and python/vol_cluster.py. Expects
VIP_POOL (two addresses, db-postgres-recovery.nix) spliced in ahead of
this file.
"""

import shlex

PORT = 5432       # VIP-exposed, client-facing port
PG_PORT = 55432   # postgres's own internal listen port, reachable directly
DATABASE = "appdb"
REPL_PASSWORD = "repl-s3cret"
SUPER_PASSWORD = "super-s3cret"
REPLICAS = 3
STATIC_UID = 8332  # internal/blocks/pgha.StaticUID -- pgdata's owning uid

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


form("pgrc")
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
    primary_name = find_primary(nodes, timeout=120)
    primary_idx = placement_index(b, primary_name)
    assert primary_idx is not None, f"no live placement for {primary_name}: {b.get('status')}"
    print(f"current primary: {primary_name} (replica {primary_idx})")

with subtest("real, indexable data to check: pgbench's own tables plus one known row"):
    client.succeed(
        f"PGPASSWORD={SUPER_PASSWORD} pgbench -h {VIP} -p {PORT} -U postgres -i --scale=1 {DATABASE}"
    )
    psql_client(VIP, PORT, "CREATE TABLE recovery_check (id serial primary key, val text)")
    psql_client(VIP, PORT, "INSERT INTO recovery_check (val) VALUES ('before-kill')")

with subtest("kill the primary's whole VM"):
    NODE_BY_NAME[primary_name].crash()

with subtest("a surviving replica promotes itself automatically"):
    survivor_names = [n for n in nodes if n != primary_name]
    new_primary_name = find_primary(survivor_names, timeout=90)
    print(f"new primary: {new_primary_name}")

with subtest("amcheck reports zero corruption on the promoted primary (X3)"):
    psql_client(VIP, PORT, "CREATE EXTENSION IF NOT EXISTS amcheck")
    # Every btree index in the public schema -- pgbench's own tables
    # (pgbench_branches/tellers/accounts) plus recovery_check, all
    # created above. bt_index_check raises on any detected corruption,
    # so psql's own ON_ERROR_STOP makes a non-zero rc the failure signal.
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
    # enough to read pgdata's own files here (found across two earlier
    # attempts): both the VOLUME directory chain ABOVE pgdata (mount's
    # own parent and mount itself, created by the root-run storage
    # layer, not postgres) and pgdata's own contents need read/traverse
    # granted to be safely reachable regardless of whatever owner/group
    # postgres actually got. postgres itself refuses to START against a
    # data directory MODE looser than 0700/0750, though -- restored to
    # 0700 right after the check, before the restart below.
    volume_root = mount.removesuffix("/mnt")
    new_primary_m.succeed(f"chmod o+rX {volume_root} {mount} && chmod -R o+rX {pgdata}")
    # pg_checksums requires the cluster cleanly shut down -- a brief,
    # deliberate stop/check/restart of the SAME managed unit, not a
    # lasting disruption: by this point in the test nothing but this
    # check is still exercising the promoted primary's own write path.
    # Stopped by signalling postgres's own PID directly, as root -- two
    # other approaches were tried and failed here: `pg_ctl stop` under
    # setpriv got "Operation not permitted" signalling the PID (kill()'s
    # own uid match, root sidesteps it outright); `systemctl stop` raced
    # its own KillMode=control-group default -- this unit's real main
    # process is expanse-block-run (postgres is its own separate child in
    # the same cgroup, not an exec() replacement), so once THAT exits,
    # systemd considers the unit stopped and SIGKILLs whatever's left in
    # the cgroup, catching postgres's own graceful shutdown mid-flight
    # and leaving pg_control marked unclean ("must be shut down"). SIGINT
    # is a postmaster fast shutdown, the same signal pg_ctl -m fast sends.
    pg_pid = new_primary_m.succeed(f"head -1 {pgdata}/postmaster.pid").strip()
    new_primary_m.succeed(f"kill -INT {pg_pid}")
    new_primary_m.succeed(f"timeout 30 sh -c 'while kill -0 {pg_pid} 2>/dev/null; do sleep 0.5; done'")
    out = new_primary_m.succeed(as_pguser(f"pg_checksums -D {pgdata} -c"))
    print(f"[diag] pg_checksums: {out}")
    # Restore pgdata's own strict mode -- postgres's restart below
    # refuses to start otherwise (its own permissions check looks only
    # at this one top-level directory, not its contents, so this alone
    # is sufficient; the loosened file-level bits underneath are inert).
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

print("DB-POSTGRES-RECOVERY DONE")
