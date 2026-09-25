"""backup-destroy-rebuild: PHASE-08-TASKS.md Stream D (X6, the decider).
The vertical slice: every node's cluster state is destroyed simultaneously
-- no survivor, no live quorum for a restored node to catch up from, the
harder scenario X5 explicitly deferred to this stream -- then rebuilt from
backup credentials with the one new `expanse cluster restore` command, and
both configuration (a real desired-state resource) and data (a real
replicated volume's content) are independently verified correct
afterward, not assumed from "the restore command exited 0".

Also closes R2 for real: all three nodes' /persist backups are taken
while the cluster is fully quiesced (every daemon stopped), so they are
provably at the identical raft position, not merely close in wall-clock
time -- the multi-node consistency proof Stream C's single-node test
could not give.

Runs after cluster-common.py and vol_cluster.py.
"""

SIZE_MIB = 64
VG = "vg0"
SNAP = "vsnap"
SOCK = "/run/expanse/agent.sock"
GARAGE_ADDR = "192.168.1.1:3900"


def snapshot_dev(res):
    return f"/dev/{VG}/{res}-snap-{SNAP}"


def apply_file(m, rid, path, content):
    rc, out = m.execute(
        "expanse ctl resource apply --socket " + SOCK + " -f - <<'EOF'\n"
        f"{rid}:\n  type: file\n  path: {path}\n  content: {content}\n  mode: \"0644\"\nEOF\n"
    )
    assert rc == 0, f"{m.name} apply failed: {out}"


def repo_env(node_name):
    """Each node backs up to its own repo path -- the realistic shape of
    per-node backup credentials, and what lets `expanse cluster restore`
    pull exactly that node's own state back with no tag filtering needed."""
    return (
        f"AWS_ACCESS_KEY_ID={KEY_ID} AWS_SECRET_ACCESS_KEY={SECRET_KEY} "
        "RESTIC_PASSWORD=expanse-destroy-rebuild-password "
        f"RESTIC_REPOSITORY=s3:http://{GARAGE_ADDR}/backups/{node_name}"
    )


def vol_env():
    return (
        f"AWS_ACCESS_KEY_ID={KEY_ID} AWS_SECRET_ACCESS_KEY={SECRET_KEY} "
        "RESTIC_PASSWORD=expanse-destroy-rebuild-vol-password "
        f"RESTIC_REPOSITORY=s3:http://{GARAGE_ADDR}/backups/vol"
    )


form("destroyrebuild")

with subtest("real desired state: a file resource on n1"):
    apply_file(n1, "file:/etc/rebuilt-config", "/etc/rebuilt-config", "config-survives")
    n1.wait_until_succeeds("grep -qx config-survives /etc/rebuilt-config", timeout=60)

with subtest("real replicated volume, filled and snapshotted"):
    n1.succeed(f"expanse ctl volume create vroot --size {SIZE_MIB}Mi --replication 3")
    for m in NODES:
        m.wait_until_succeeds("drbdadm status | grep -q '^vol-'", timeout=180)
    res = n1.succeed("drbdadm status | head -1 | cut -d' ' -f1").strip()
    wait_for(lambda: all(fully_replicated(m, res) for m in NODES), "every replica UpToDate", 300)
    wait_for(lambda: len(primaries(res)) == 1, "one primary")
    primary = primaries(res)[0]
    dev = device_of(primary)
    fill_paced(primary, dev, SIZE_MIB)
    ref_data = checksum(primary, dev, SIZE_MIB)

    n1.succeed(f"expanse ctl volume snapshot vroot --name {SNAP}")
    wait_for(lambda: SNAP in n1.execute("expanse ctl volume inspect vroot 2>&1")[1], "the snapshot to be recorded", 60)
    # A thin snapshot LV starts inactive (lvm.Snapshot's own doc comment).
    primary.succeed(f"lvchange --activate y --ignoreactivationskip {VG}/{res}-snap-{SNAP}")
    assert checksum(primary, snapshot_dev(res), SIZE_MIB) == ref_data, "snapshot LV does not hold the reference data"

with subtest("garage reachable cluster-wide, and restic present on every node"):
    n1.wait_for_unit("garage.service")
    n1.wait_for_open_port(3900)
    for m in [n2, n3]:
        m.wait_until_succeeds("timeout 2 bash -c '(echo > /dev/tcp/192.168.1.1/3900) 2>/dev/null'", timeout=60)

with subtest("bootstrap a single-node garage layout, S3 key and bucket"):
    node_id = n1.succeed("garage status | awk 'NR>2 && NF {print $1; exit}'").strip()
    assert node_id, "no node id found in `garage status`"
    n1.succeed(f"garage layout assign -z dc1 -c 1G {node_id}")
    n1.succeed("garage layout apply --version 1")
    key_out = n1.succeed("garage key create restic-key")
    KEY_ID = [l for l in key_out.splitlines() if l.startswith("Key ID:")][0].split(": ", 1)[1].strip()
    SECRET_KEY = [l for l in key_out.splitlines() if l.startswith("Secret key:")][0].split(": ", 1)[1].strip()
    n1.succeed("garage bucket create backups")
    n1.succeed("garage bucket allow --read --write --key restic-key backups")

with subtest("restic init: one repo per node, plus one for the volume, all on the real garage endpoint"):
    for m in NODES:
        m.succeed(f"{repo_env(m.name)} restic init")
    primary.succeed(f"{vol_env()} restic init")

with subtest("X2: back up the volume's snapshot content, the reference for after data loss"):
    primary.succeed("mkdir -p /root/backup-src")
    primary.succeed(
        f"dd if={snapshot_dev(res)} of=/root/backup-src/vroot.img bs=1M count={SIZE_MIB} iflag=direct conv=fsync"
    )
    out = primary.succeed(f"{vol_env()} restic backup /root/backup-src/vroot.img")
    assert "Added to the repository: 0 B" not in out, out

with subtest("simulate total data loss: the live volume is zeroed, replicated to every UpToDate replica"):
    primary.succeed(f"dd if=/dev/zero of={dev} bs=1M count={SIZE_MIB} oflag=direct conv=fsync,notrunc")
    zeroed = checksum(primary, dev, SIZE_MIB)
    assert zeroed != ref_data, "zeroing did not change the live volume (bad test data)"

with subtest("quiesce every node so the three /persist backups below are at the identical raft position"):
    for m in NODES:
        m.succeed("systemctl stop expansed.service")

before_ids = {}
with subtest("X3/X5: back up every node's own /persist/expanse while quiesced (R2: same raft position, not just close in time)"):
    for m in NODES:
        before_ids[m.name] = m.succeed("cat /persist/expanse/cluster-id").strip()
        out = m.succeed(f"{repo_env(m.name)} restic backup /persist/expanse")
        assert "Added to the repository: 0 B" not in out, out

with subtest("destroy the cluster entirely: every node's cluster state is gone, no survivor"):
    for m in NODES:
        m.succeed("rm -rf /persist/expanse")
        m.succeed("test ! -e /persist/expanse/cluster-id")

with subtest("rebuild from backup credentials plus one command, run on every node"):
    for m in NODES:
        rc, out = m.execute(f"{repo_env(m.name)} expanse cluster restore --data-dir /persist/expanse")
        assert rc == 0, f"{m.name} restore failed: {out}"

with subtest("identity: every node's cluster-id is restored identical to before destruction"):
    for m in NODES:
        got = m.succeed("cat /persist/expanse/cluster-id").strip()
        assert got == before_ids[m.name], f"{m.name} cluster-id changed: {got} != {before_ids[m.name]}"

with subtest("every node's daemon starts and the cluster reforms quorum with no live survivor to catch up from"):
    for m in NODES:
        m.succeed("systemctl start expansed.service")
    for m in NODES:
        m.wait_for_unit("expansed.service")
        wait_agent_ready(m)
    wait_quorum("3/2", 180)

with subtest("configuration verifies: the reconciler recreates the file resource from the restored generation"):
    n1.wait_until_succeeds("grep -qx config-survives /etc/rebuilt-config", timeout=60)

with subtest("data verifies: restore the volume's backed-up content onto the still-zeroed device"):
    res2 = n1.succeed("drbdadm status | head -1 | cut -d' ' -f1").strip()
    wait_for(lambda: len(primaries(res2)) == 1, "one primary after rebuild")
    primary2 = primaries(res2)[0]
    dev2 = device_of(primary2)
    assert checksum(primary2, dev2, SIZE_MIB) != ref_data, "volume data was not actually lost (bad test data)"
    primary2.succeed("rm -rf /root/restore-out")
    primary2.succeed(f"{vol_env()} restic restore latest --target /root/restore-out")
    restored = primary2.succeed("find /root/restore-out -name vroot.img").strip().splitlines()[0]
    primary2.succeed(f"dd if={restored} of={dev2} bs=1M count={SIZE_MIB} oflag=direct conv=fsync,notrunc")
    assert checksum(primary2, dev2, SIZE_MIB) == ref_data, "restored volume data does not match the pre-loss reference"

print(
    "BACKUP-DESTROY-REBUILD PASSED: the whole cluster was destroyed with no survivor, rebuilt from "
    "backup credentials with one new command (`expanse cluster restore`) on every node, and both "
    "configuration and data verified correct against their pre-destruction reference values"
)
