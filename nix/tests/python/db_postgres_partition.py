"""PHASE-05-TASKS.md R3 (split-brain prevention, D5): a real network
partition -- not a hard .crash() -- isolates the current primary's node
from its two peers (nftables, both directions, cluster-partition.nix's
own recipe applied to db/postgres). Unlike every other db-postgres VM
test in this suite, the partitioned node's agent process is NEVER
restarted, so this is the only test that actually exercises pgha's own
held.Done()-watch self-fencing path (internal/blocks/pgha's
reconcileOne): the surviving majority must elect a new primary within
the lease's own guard band, a direct write against the isolated primary
(bypassing the VIP entirely) must never be acknowledged --
synchronous_standby_names has no reachable standby to confirm it -- and
once the partition heals, the old primary must rejoin as a fresh
streaming replica with zero divergence, reusing the same X4 path Stream
C already proved for a hard kill.

Runs after cluster-common.py, client-common.py (with `client` bound to
the external VM), block-common.py and python/vol_cluster.py. Expects
VIP_POOL (two addresses, db-postgres-partition.nix) spliced in ahead of
this file.
"""

import shlex

PORT = 5432       # VIP-exposed, client-facing port
PG_PORT = 55432   # postgres's own internal listen port, reachable directly
DATABASE = "appdb"
REPL_PASSWORD = "repl-s3cret"
SUPER_PASSWORD = "super-s3cret"
REPLICAS = 3

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
    """Run one SQL statement from the external client, over the VIP."""
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
    VIP/LB entirely."""
    cmd = (f"PGPASSWORD={SUPER_PASSWORD} timeout {timeout} psql -h {node_ip} -p {PG_PORT} -U postgres -d {DATABASE} "
           f"-v ON_ERROR_STOP=1 -tA -c {shlex.quote(sql)}")
    rc, out = client.execute(cmd)
    return rc, out.strip()


def find_primary(node_names, timeout):
    """Poll node_names directly until exactly one reports
    pg_is_in_recovery()=f. Never touches a crashed node -- irrelevant
    here since partition() never crash()es anything, only isolates it at
    the network layer; the isolated node stays a perfectly valid
    NixOS test-driver machine object to query throughout."""
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
    for p in (b.get("status") or {}).get("placements", []):
        if p.get("nodeId") == node_name and p.get("phase") != "LOST" and p.get("replicaIndex", 0) >= 0:
            return p.get("replicaIndex", 0)
    return None


def replica_mount(m, idx):
    vname = f"blk-default-pg-pgdata-{idx}"
    row = volume_row(m, vname)
    assert row, f"no volume {vname} visible on {m.name} yet"
    return f"/var/lib/expanse/volumes/{row['id']}/mnt"


def wait_row_replicates(m, sockdir, sql, want, timeout=60):
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


def partition_from(isolated, peers):
    """nftables-isolate isolated from every node in peers, both
    directions -- cluster-partition.nix's own recipe, applied here to
    db/postgres's own replication traffic (which dials the SAME
    lookupNodeIP/raft_addr address raft itself uses, not a separate
    overlay -- confirmed directly against internal/agent/lb.go, not
    assumed)."""
    m = NODE_BY_NAME[isolated]
    rules = "nft add table ip exppgpart; nft add chain ip exppgpart output '{ type filter hook output priority 0; }'; "
    rules += "".join(f"nft add rule ip exppgpart output ip daddr {IP[p]} drop; " for p in peers)
    rules += "nft add chain ip exppgpart input '{ type filter hook input priority 0; }'; "
    rules += "".join(f"nft add rule ip exppgpart input ip saddr {IP[p]} drop; " for p in peers)
    m.succeed(rules)


def heal(isolated):
    NODE_BY_NAME[isolated].succeed("nft delete table ip exppgpart")


form("pgpt")
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
    survivor_names = [n for n in nodes if n != primary_name]
    print(f"current primary: {primary_name} (replica {primary_idx}); survivors: {survivor_names}")

with subtest("seed a row to check for divergence after the partition heals"):
    psql_client(VIP, PORT, "CREATE TABLE partition_check (id serial primary key, val text)")
    psql_client(VIP, PORT, "INSERT INTO partition_check (val) VALUES ('before-partition')")

with subtest("partition the primary's node from both survivors (nftables, not .crash())"):
    t0 = time.time()
    partition_from(primary_name, survivor_names)

with subtest("the surviving majority elects a new primary within the guard band (D5)"):
    new_primary_name = find_primary(survivor_names, timeout=90)
    reconverge_s = time.time() - t0
    print(f"new primary {new_primary_name} elected after {reconverge_s:.1f}s (partitioned, not crashed)")

with subtest("a direct write against the isolated primary is never acknowledged (D5)"):
    # Bypasses the VIP entirely -- dials the isolated node's own port
    # directly, proving the write path itself is closed, not just that
    # clients can no longer find it through the VIP. With every peer
    # unreachable, synchronous_standby_names = 'ANY 1 (*)' has no
    # standby left to confirm a commit, so this must hang until psql's
    # own timeout kills it, never return success.
    rc, out = node_psql(IP[primary_name], "INSERT INTO partition_check (val) VALUES ('during-partition')", timeout=15)
    assert rc != 0, f"a direct write to the isolated primary was acknowledged during the partition: {out}"
    print(f"[diag] direct write to the isolated primary correctly never completed: rc={rc} out={out!r}")

with subtest("heal the partition"):
    heal(primary_name)

with subtest("the old primary rejoins as a streaming replica, not diverged (X4 reused under a real partition)"):
    # Generous budget, matching db_postgres_recovery.py's own: pgha's
    # own held.Done()-watch (this test's own regression target) must
    # notice the lost lease, fall through to reclaimPrimary, discover
    # the new primary once the store is reachable again, and demote --
    # then cmd/expanse-block-run's watchForDemotion stops and wipes
    # PGDATA, and systemd re-bootstraps it fresh via pg_basebackup.
    wait_is_replica(primary_name, timeout=240)
    wait_replica_count(new_primary_name, REPLICAS - 1, timeout=120)
    print(f"{primary_name} rejoined as a streaming replica of {new_primary_name}, no manual pg_rewind")

with subtest("the unacknowledged during-partition write did not survive; zero divergence"):
    # This project's own correctness bar is "no ACKNOWLEDGED write
    # lost," not "no write ever locally attempted" -- X4's own
    # wipe-and-reclone design (ARCHITECTURE.md A31) discards whatever
    # the isolated primary held locally, unconditionally, once it
    # rejoins, by design.
    count = psql_client(VIP, PORT, "SELECT count(*) FROM partition_check WHERE val = 'during-partition'")
    assert count == "0", f"the unacknowledged during-partition write survived onto the surviving cluster: count={count}"

    psql_client(VIP, PORT, "INSERT INTO partition_check (val) VALUES ('after-heal')")
    sockdir = replica_mount(NODE_BY_NAME[primary_name], primary_idx) + "/.expanse-postgres/sock"
    wait_row_replicates(NODE_BY_NAME[primary_name], sockdir,
                         "SELECT val FROM partition_check WHERE val = 'after-heal'", "after-heal")
    print("verified zero divergence: no unacknowledged write survived, and the rejoined replica streams new writes")

print("DB-POSTGRES-PARTITION DONE")
