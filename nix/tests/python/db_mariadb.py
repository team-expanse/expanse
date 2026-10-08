"""db/mariadb: a MariaDB that survives losing the node serving it.

Deploys a SINGLETON db/mariadb block on a 3-way volume behind a VIP. The
client commits rows over the MySQL protocol and runs a writer that inserts
one numbered row per autocommit transaction; the serving VM is crashed
mid-write. The block, its DRBD primary and its VIP must re-converge on one
survivor, the writer must resume, and every row the client saw committed
must read back.

Runs after cluster-common.py (with client bound to n9), block-common.py
and vol_cluster.py.
"""

import shlex

PORT = 3306  # client-facing, on the VIP
DB_PORT = 13306  # mariadbd's own port: the VIP holder binds PORT
ROOT_PW = "root-secret'1"
APP_PW = "app-secret"
VOL_NAME = "blk-default-appdb-appdb-data"  # storage.BlockVolumeName(ns, block, storageName)
MACHINES = {"n1": n1, "n2": n2, "n3": n3}

MANIFEST = f"""apiVersion: expanse.io/v1
kind: Block
metadata:
  name: appdb
  namespace: default
spec:
  type: db/mariadb
  replicas: 1
  strategy:
    kind: SINGLETON
  resources:
    requests:
      cpu: 100m
      memory: 256Mi
  storage:
    - name: appdb-data
      size: 256Mi
      replication: 3
      mountPath: /var/lib/mariadb
  config:
    port: {DB_PORT}
    rootPassword: "{ROOT_PW}"
    database: app
    user: app
    password: {APP_PW}
  network:
    ports:
      - name: mysql
        port: {PORT}
        target_port: {DB_PORT}
        protocol: tcp
        expose: EXPOSE_VIP
    health_check:
      readiness:
        type: PROBE_TCP
        port: {DB_PORT}
        period_seconds: 2
"""


def sql(query, user="app", password=APP_PW, db="app", host=None):
    """A mariadb client invocation against the block's VIP, tab-separated without headers."""
    return (f"mariadb -h {host or VIP} -P {PORT} -u {user} --password={shlex.quote(password)} "
            f"--connect-timeout=5 -N -B {db} -e {shlex.quote(query)}")


def vip_holders(vip, machines):
    """Nodes among machines carrying vip; never pass a crashed one (it would reboot)."""
    return [m.name for m in machines
            if m.execute(f"ip -4 -o addr show eth1 | grep -qF ' {vip}/'")[0] == 0]


def host_mount(m):
    row = volume_row(m, VOL_NAME)
    return f"/var/lib/expanse/volumes/{row['id']}/mnt" if row else None


def last_acked():
    out = client.execute("cat /root/last_acked 2>/dev/null || echo 0")[1].strip()
    return int(out) if out.isdigit() else 0


def wait_acked(n, timeout, what):
    deadline = time.time() + timeout
    while time.time() < deadline:
        if last_acked() >= n:
            return last_acked()
        time.sleep(1)
    raise AssertionError(f"{what}: only {last_acked()} committed rows in {timeout}s (want >= {n})")


def journal(m):
    return m.execute("journalctl -u 'expanse-block@default-appdb-0.service' --no-pager -n 80 2>&1")[1]


form("mariadb")
for m in MACHINES.values():
    wait_agent_ready(m)

with subtest("a manifest without a root password is rejected"):
    bad = MANIFEST.replace(f'    rootPassword: "{ROOT_PW}"\n', "")
    n1.succeed(f"echo {base64.b64encode(bad.encode()).decode()} | base64 -d > /tmp/bad.yaml")
    out = n1.fail(f"expanse ctl block apply {SOCK} -f /tmp/bad.yaml 2>&1")
    assert "rootPassword" in out, f"rejection does not name rootPassword: {out}"

with subtest("deploy a SINGLETON db/mariadb block on a replicated volume"):
    deploy(n1, "appdb", MANIFEST)
    b = wait_phase(n1, "appdb", ["RUNNING"], 180)
    nodes = placement_nodes(b)
    assert len(nodes) == 1, f"appdb placed on {nodes}: {b.get('status')}"
    holder = next(iter(nodes))
    VIP = wait_block_vip(n1, "appdb")

with subtest("every volume replica is UpToDate and the VIP sits on the holder"):
    for m in NODES:
        m.wait_until_succeeds("drbdadm status | grep -q '^vol-'", timeout=180)
    res = n1.succeed("drbdadm status | head -1 | cut -d' ' -f1").strip()
    wait_for(lambda: all(fully_replicated(m, res) for m in NODES), "every replica UpToDate")
    wait_for(lambda: vip_holders(VIP, NODES) == [holder], f"VIP {VIP} on {holder}", timeout=60)

with subtest("the app user creates a table and commits through the VIP"):
    try:
        client.wait_until_succeeds(sql("SELECT 1"), timeout=120)
    except Exception:
        h = MACHINES[holder]
        print(client.execute(sql("SELECT 1") + " 2>&1")[1])
        print(h.execute(f"ss -ltnp 2>&1 | grep -E '{PORT}|{DB_PORT}'; ip -4 -o addr show eth1")[1])
        print(journal(h))
        raise
    client.succeed(sql("CREATE TABLE seq (i INT PRIMARY KEY) ENGINE=InnoDB"))
    client.succeed(sql("CREATE TABLE notes (k VARCHAR(16) PRIMARY KEY, v TEXT) ENGINE=InnoDB"))
    client.succeed(sql("START TRANSACTION; INSERT INTO notes VALUES ('a', 'alpha'), ('b', 'beta'); COMMIT"))
    durable = client.succeed(sql("SELECT @@innodb_flush_log_at_trx_commit, @@innodb_doublewrite")).split()
    assert durable == ["1", "ON"], f"commits are not synced: {durable}"

with subtest("accounts: root needs its password, the app user stays in its database"):
    client.succeed(sql("SELECT COUNT(*) FROM mysql.user", user="root", password=ROOT_PW, db="mysql"))
    client.fail(sql("SELECT 1", user="root", password="wrong", db="mysql"))
    client.fail(sql("CREATE DATABASE other"))
    # mariadb-install-db's passwordless root@127.0.0.1 must not survive.
    MACHINES[holder].fail(f"mariadb -h 127.0.0.1 -P {DB_PORT} -u root --skip-password -e 'SELECT 1'")

with subtest("the datadir lives on the volume"):
    mnt = host_mount(MACHINES[holder])
    MACHINES[holder].succeed(f"test -d {mnt}/mariadb/mysql && test -s {mnt}/mariadb/ibdata1")

with subtest("a continuous writer commits numbered rows"):
    client.succeed(
        "cat > /root/writer.sh << 'EOF'\n"
        "export PATH=/run/current-system/sw/bin:$PATH\n"
        "i=1\n"
        "while true; do\n"
        # Only a successful autocommit INSERT counts; a duplicate means it landed last time.
        f"  out=$(mariadb -h {VIP} -P {PORT} -u app --password={APP_PW} --connect-timeout=5 app "
        "-e \"INSERT INTO seq VALUES ($i)\" 2>&1)\n"
        "  if [ $? -eq 0 ] || echo \"$out\" | grep -q 'Duplicate entry'; then\n"
        "    echo $i > /root/last_acked; i=$((i+1))\n"
        "  fi\n"
        "  sleep 0.2\n"
        "done\n"
        "EOF\n"
    )
    client.succeed("systemd-run --unit=db-writer /bin/sh /root/writer.sh")
    wait_acked(20, 90, "pre-kill warmup")

with subtest("kill the serving node's VM mid-write"):
    acked_at_kill = last_acked()
    t0 = time.time()
    MACHINES[holder].crash()
    survivors = [m for n, m in MACHINES.items() if n != holder]

with subtest("block, volume primary and VIP re-converge on one survivor"):
    new_holder = None
    last_seen = {}
    deadline = time.time() + 240
    while time.time() < deadline and new_holder is None:
        cur = placement_nodes(get_json(survivors[0], "appdb") or {})
        if len(cur) == 1 and holder not in cur:
            cand = next(iter(cur))
            last_seen = {"placement": cand, "role": role_of(MACHINES[cand], res),
                         "vip": vip_holders(VIP, survivors)}
            if last_seen["role"] == "Primary" and last_seen["vip"] == [cand]:
                new_holder = cand
        else:
            last_seen = {"placement": sorted(cur)}
        time.sleep(2)
    assert new_holder, f"never re-converged on one survivor in 240s: {last_seen}"
    print(f"appdb re-converged on {new_holder} after {time.time() - t0:.1f}s")

with subtest("the writer resumes against the same endpoint and account"):
    try:
        resumed = wait_acked(acked_at_kill + 5, 240, "post-failover commits")
    except AssertionError:
        print(journal(MACHINES[new_holder]))
        raise
    print(f"writer resumed: {resumed} committed (was {acked_at_kill} at kill) "
          f"{time.time() - t0:.1f}s after the crash")

with subtest("every committed row reads back"):
    client.succeed("systemctl stop db-writer")
    acked = last_acked()
    got = client.succeed(sql(f"SELECT COUNT(*) FROM seq WHERE i <= {acked}")).strip()
    assert got == str(acked), f"{got} of {acked} committed rows survived"
    notes = client.succeed(sql("SELECT v FROM notes ORDER BY k")).split()
    assert notes == ["alpha", "beta"], f"pre-failover transaction lost: {notes}"
    client.succeed(sql("SELECT 1", user="root", password=ROOT_PW, db="mysql"))
    print(f"{acked} committed rows and the pre-failover transaction intact after failover")
