"""util/restic-backup: scheduled backups of a volume follow its primary and restore.

Deploys a SINGLETON db/mariadb block on a 3-way volume and a DAEMONSET
util/restic-backup block pointed at garage on n9. Only the replica on the
volume's primary may back it up; after that node is crashed the survivor's
replica must take over, retention must prune, and the database must come back
from the repository with exactly the rows the last backup held.

Runs after cluster-common.py (with client bound to n9), block-common.py and
vol_cluster.py.
"""

import shlex

PORT, DB_PORT = 3306, 13306
APP_PW = "app-secret"
VOL = "blk-default-appdb-appdb-data"  # storage.BlockVolumeName(ns, block, storageName)
MACHINES = {"n1": n1, "n2": n2, "n3": n3}
STATUS_PORT = 18900
RESTIC_PW = "backup-pass-123"

DB_MANIFEST = f"""apiVersion: expanse.io/v1
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
    rootPassword: root-secret
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


def backup_manifest(key_id, secret):
    return f"""apiVersion: expanse.io/v1
kind: Block
metadata:
  name: backup
  namespace: default
spec:
  type: util/restic-backup
  strategy:
    kind: DAEMONSET
  resources:
    requests:
      cpu: 100m
      memory: 128Mi
  config:
    repository: s3:http://n9:3900/backups
    password: {RESTIC_PW}
    env:
      AWS_ACCESS_KEY_ID: {key_id}
      AWS_SECRET_ACCESS_KEY: {secret}
    volumes:
      - appdb/appdb-data
    interval: 1m
    keep:
      last: 3
  network:
    health_check:
      readiness:
        type: PROBE_HTTP
        path: /healthz
        port: {STATUS_PORT}
        period_seconds: 5
"""


def sql(query):
    return (f"mariadb -h {VIP} -P {PORT} -u app --password={APP_PW} --connect-timeout=5 -N -B app "
            f"-e {shlex.quote(query)}")


def rows():
    return client.succeed(sql("SELECT note FROM notes ORDER BY id")).split()


def snapshots():
    """The repository's snapshots of VOL, oldest first, read from n9; none before the block creates it."""
    rc, out = client.execute(f"{RESTIC_ENV} restic snapshots --no-lock --json --host {VOL} 2>/dev/null")
    if rc == 10:  # restic: the repository does not exist
        return []
    assert rc == 0, f"restic snapshots exited {rc}: {out}"
    return sorted(json.loads(out), key=lambda s: s["time"])


def snapshot_epoch(s):
    """restic's RFC 3339 time with nanoseconds, as epoch seconds."""
    return int(client.succeed(f"date -d {shlex.quote(s['time'])} +%s").strip())


def wait_snapshot_after(t, what, timeout=240):
    deadline = time.time() + timeout
    while time.time() < deadline:
        for s in snapshots():
            SEEN.add(s["id"])
            if snapshot_epoch(s) > t:
                return s
        time.sleep(5)
    raise AssertionError(f"no backup of {VOL} after {what} in {timeout}s; replicas: "
                         f"{ {n: replica_status(m) for n, m in MACHINES.items() if n not in CRASHED} }")


def replica_status(m):
    rc, out = m.execute(f"curl -s -m 5 http://127.0.0.1:{STATUS_PORT}/")
    return json.loads(out).get(VOL, {}) if rc == 0 and out.startswith("{") else {}


def journal(m):
    return m.execute("journalctl -u 'expanse-block-root@default-backup-*' --no-pager -n 60 2>&1")[1]


def holder_of_db():
    """The node serving appdb, or "" while it has no single live placement."""
    nodes = placement_nodes(get_json(survivors[0], "appdb") or {})
    return next(iter(nodes)) if len(nodes) == 1 else ""


def db_moved_from(old):
    new = holder_of_db()
    return new not in ("", old) and role_of(MACHINES[new], res) == "Primary"


SEEN = set()
CRASHED = set()
survivors = list(MACHINES.values())

form("restic")
for m in MACHINES.values():
    wait_agent_ready(m)

with subtest("garage on n9 serves an empty bucket"):
    client.wait_for_unit("garage.service")
    client.wait_for_open_port(3900)
    node_id = client.succeed("garage status | awk 'NR>2 && NF {print $1; exit}'").strip()
    client.succeed(f"garage layout assign -z dc1 -c 1G {node_id} && garage layout apply --version 1")
    key_out = client.succeed("garage key create restic-key")
    KEY_ID = next(x for x in key_out.splitlines() if x.startswith("Key ID:")).split(": ", 1)[1].strip()
    SECRET = next(x for x in key_out.splitlines() if x.startswith("Secret key:")).split(": ", 1)[1].strip()
    client.succeed("garage bucket create backups && garage bucket allow --read --write --key restic-key backups")
    RESTIC_ENV = (f"AWS_ACCESS_KEY_ID={KEY_ID} AWS_SECRET_ACCESS_KEY={SECRET} "
                  f"RESTIC_PASSWORD={RESTIC_PW} RESTIC_REPOSITORY=s3:http://n9:3900/backups")

with subtest("a manifest without a repository password is rejected"):
    bad = backup_manifest(KEY_ID, SECRET).replace(f"    password: {RESTIC_PW}\n", "")
    n1.succeed(f"echo {base64.b64encode(bad.encode()).decode()} | base64 -d > /tmp/bad.yaml")
    out = n1.fail(f"expanse ctl block apply {SOCK} -f /tmp/bad.yaml 2>&1")
    assert "password" in out, f"rejection does not name password: {out}"

with subtest("deploy db/mariadb and commit rows"):
    deploy(n1, "appdb", DB_MANIFEST)
    wait_phase(n1, "appdb", ["RUNNING"], 240)
    VIP = wait_block_vip(n1, "appdb")
    client.wait_until_succeeds(sql("CREATE TABLE IF NOT EXISTS notes (id INT AUTO_INCREMENT PRIMARY KEY, note TEXT)"),
                               timeout=120)
    client.succeed(sql("INSERT INTO notes (note) VALUES ('first')"))
    res = n1.succeed("drbdadm status | head -1 | cut -d' ' -f1").strip()
    holder = holder_of_db()
    assert holder, "appdb has no single placement"

with subtest("the replica on the volume's primary backs it up, and no other"):
    t0 = int(client.succeed("date +%s").strip())
    deploy(n1, "backup", backup_manifest(KEY_ID, SECRET))
    wait_placement_nodes(n1, "backup", 3, 180)
    try:
        wait_snapshot_after(t0, "deploying the backup block")
    except AssertionError:
        print(journal(MACHINES[holder]))
        raise
    for name, m in MACHINES.items():
        wait_for(lambda m=m: replica_status(m) != {}, f"{name}'s status endpoint", timeout=60)
        st = replica_status(m)
        assert st.get("primary") == (name == holder), f"{name} status {st}; primary is on {holder}"
    assert "lastBackup" in replica_status(MACHINES[holder]), replica_status(MACHINES[holder])
    MACHINES[holder].succeed(f"curl -sf http://127.0.0.1:{STATUS_PORT}/healthz")
    # A backup holds its snapshot LV only while it runs.
    wait_for(lambda: "-snap-restic" not in MACHINES[holder].succeed("lvs --noheadings -o lv_name"),
             "the backup's snapshot LV dropped", timeout=60)

with subtest("the next backup carries a newer row"):
    client.succeed(sql("INSERT INTO notes (note) VALUES ('second')"))
    wait_snapshot_after(int(client.succeed("date +%s").strip()), "the second row")

with subtest("after the primary's node crashes, the survivor's replica takes over"):
    t_crash = int(client.succeed("date +%s").strip())
    MACHINES[holder].crash()
    CRASHED.add(holder)
    survivors = [m for n, m in MACHINES.items() if n not in CRASHED]
    wait_for(lambda: db_moved_from(holder), "appdb and its primary on a survivor", timeout=240)
    new_holder = holder_of_db()
    wait_snapshot_after(t_crash, "the crash")
    st = replica_status(MACHINES[new_holder])
    assert st.get("primary") and "lastBackup" in st, f"{new_holder} status {st}"
    print(f"backups resumed from {new_holder}")

with subtest("retention keeps the last 3"):
    wait_for(lambda: len(SEEN | {s["id"] for s in snapshots()}) >= 5, "five backups in all", timeout=300)
    SEEN.update(s["id"] for s in snapshots())
    kept = snapshots()
    assert len(kept) <= 3, f"keep.last=3 left {len(kept)} snapshots"
    client.succeed(f"{RESTIC_ENV} restic check")

with subtest("the database restores from the repository"):
    n_ok = survivors[0]
    n_ok.succeed(f"expanse ctl block delete {SOCK} backup")
    wait_for(lambda: all("backup" not in m.succeed("systemctl list-units --no-legend 'expanse-block-root@*'")
                         for m in survivors), "the backup replicas stopped", timeout=120)
    client.succeed(sql("INSERT INTO notes (note) VALUES ('after-backup')"))
    want = rows()
    want.remove("after-backup")
    n_ok.succeed(f"expanse ctl block delete {SOCK} appdb")
    wait_for(lambda: all("appdb" not in m.succeed("systemctl list-units --no-legend 'expanse-block*'")
                         for m in survivors), "appdb stopped", timeout=120)
    # The agent unmounts the volume once nothing holds it; writing under a mount corrupts it.
    wait_for(lambda: all(m.execute(f"findmnt /var/lib/expanse/volumes/{res}/mnt")[0] != 0 for m in survivors),
             "the volume unmounted", timeout=120)
    prim = primaries(res, survivors)
    print(f"after delete, primaries of {res}: {[m.name for m in prim]}")
    target = prim[0] if prim else survivors[0]
    if not prim:
        target.succeed(f"drbdadm primary {res}")
    dev = target.succeed(f"drbdadm sh-dev {res}").strip()
    target.succeed(f"{RESTIC_ENV} restic dump --host {VOL} latest /{VOL}.img | dd of={dev} bs=1M iflag=fullblock oflag=direct")
    if not prim:
        target.succeed(f"drbdadm secondary {res}")
    deploy(n_ok, "appdb", DB_MANIFEST)
    wait_phase(n_ok, "appdb", ["RUNNING"], 240)
    VIP = wait_block_vip(n_ok, "appdb")
    client.wait_until_succeeds(sql("SELECT 1"), timeout=120)
    got = rows()
    assert got == want, f"restored rows {got}, want the last backup's {want}"
    print("UTIL-RESTIC-BACKUP DONE")
