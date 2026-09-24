"""PHASE-05-TASKS.md Stream A: a db/postgres block, 3 active-active
replicas each with its own independent volume (D3), streaming
replication, and lease-gated primary election (D1) routed to by the
LB's PrimaryOnly mode (D2). Proves X1: deploy + an external client's
read/write queries against whichever replica is currently primary, plus
a direct check that the write actually streamed to every standby --
X2 (failover under load) is Stream B's own test, not this one.

Runs after cluster-common.py, client-common.py (with `client` bound to
the external VM), block-common.py and python/vol_cluster.py. Expects
VIP_POOL (two addresses, db-postgres.nix) spliced in ahead of this file.
"""

import shlex

PORT = 5432       # VIP-exposed, client-facing port
PG_PORT = 55432   # postgres's own internal listen port -- kept apart
# from PORT for the same reason share_smb.py's SMBD_PORT is: the VIP
# holder's own listen check reserves PORT on whichever node it moves to,
# which collides with postgres already bound to 0.0.0.0:PORT on the
# very node the VIP is trying to move TO.
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


def psql_client(host, port, sql, timeout=10):
    """Run one SQL statement from the external client, over the VIP."""
    cmd = (f"PGPASSWORD={SUPER_PASSWORD} psql -h {host} -p {port} -U postgres -d {DATABASE} "
           f"-v ON_ERROR_STOP=1 -tA -c {shlex.quote(sql)}")
    return client.succeed(cmd).strip()


def psql_local(m, sockdir, sql):
    """Run one SQL statement locally on a cluster node, over its unix
    socket (pg_hba trust -- the election controller's own admin
    channel, no password needed)."""
    cmd = f"psql -h {sockdir} -p {PG_PORT} -U postgres -d {DATABASE} -v ON_ERROR_STOP=1 -tA -c {shlex.quote(sql)}"
    return m.succeed(cmd).strip()


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


def replica_sockdir(m, idx):
    """A replica's own unix-socket directory, resolved via its
    independent per-replica volume's real host mount (D3)."""
    vname = f"blk-default-pg-pgdata-{idx}"
    row = volume_row(m, vname)
    assert row, f"no volume {vname} visible on {m.name} yet"
    return f"/var/lib/expanse/volumes/{row['id']}/mnt/.expanse-postgres/sock"


NODE_BY_NAME = {"n1": n1, "n2": n2, "n3": n3}

form("pg")
wait_agent_ready(n1)
wait_agent_ready(n2)
wait_agent_ready(n3)

with subtest("the cluster's own management UI claims one pool address"):
    # internal/agent/ui_vip.go allocates unconditionally on every
    # node.enable=true agent, independent of any block (share_smb.py's
    # own comment explains this in full).
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
    # Generous budget: 3 independent volumes must each be requested,
    # placed and healthy (D3), primary election (D1) must decide, the
    # bridge must wire each replica's own mount, and postgres itself
    # must initdb (primary) or pg_basebackup (standbys, sequenced after
    # the primary is actually listening) before the block reads RUNNING.
    deploy(n1, "pg", MANIFEST)
    b = wait_phase(n1, "pg", ["RUNNING"], 300)
    nodes = placement_nodes(b)
    assert len(nodes) == REPLICAS, f"pg placed on {nodes}, want {REPLICAS} distinct nodes: {b.get('status')}"

with subtest("the block claims the other pool address as its own VIP"):
    remaining = [a for a in VIP_POOL if a != ui_vip]
    assert len(remaining) == 1, f"VIP_POOL must have exactly 2 addresses: {VIP_POOL}"
    VIP = remaining[0]
    holder = wait_single_holder(VIP, 60, nodes)
    assert holder, f"pg's VIP ({VIP}) never settled on one of its own replicas {nodes}"

with subtest("the external client writes through the VIP, landing on the primary"):
    recovery = psql_client(VIP, PORT, "SELECT pg_is_in_recovery()")
    assert recovery == "f", f"PrimaryOnly routed a write connection to a standby: pg_is_in_recovery()={recovery}"
    psql_client(VIP, PORT, "CREATE TABLE hello (id serial primary key, msg text)")
    psql_client(VIP, PORT, "INSERT INTO hello (msg) VALUES ('hello-postgres')")

with subtest("a second, independent read through the VIP sees the write"):
    got = psql_client(VIP, PORT, "SELECT msg FROM hello WHERE id = 1")
    assert got == "hello-postgres", f"content mismatch: {got!r}"

with subtest("the write actually streamed to every replica, not just the primary"):
    for idx in range(REPLICAS):
        node_name = replica_node(b, idx)
        assert node_name, f"replica {idx} has no live placement: {b.get('status')}"
        node = NODE_BY_NAME[node_name]
        sockdir = replica_sockdir(node, idx)
        wait_row_replicates(node, sockdir, "SELECT msg FROM hello WHERE id = 1", "hello-postgres")

print("DB-POSTGRES DONE")
