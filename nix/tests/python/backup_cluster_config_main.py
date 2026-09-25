"""backup-cluster-config: PHASE-08-TASKS.md Stream C (X4, X5). Cluster
configuration -- the generations store's own desired-state history -- and
cluster identity material (cluster ID, secret, CA, node TLS, all already
living under dataDir per D2's finding) are both captured and provably
restorable through a real restic/S3 round trip, not just "the command
exited 0".

Runs after cluster-common.py; NODES/n1/n2/n3/form/wait_quorum/
wait_agent_ready all come from that shared file.
"""

SOCK = "/run/expanse/agent.sock"


def apply_file(m, rid, path, content):
    rc, out = m.execute(
        "expanse ctl resource apply --socket " + SOCK + " -f - <<'EOF'\n"
        f"{rid}:\n  type: file\n  path: {path}\n  content: {content}\n  mode: \"0644\"\nEOF\n"
    )
    assert rc == 0, f"{m.name} apply failed: {out}"


def identity_files(m):
    """sha256sum of every X5-named cluster-identity file, keyed by path."""
    files = [
        "cluster-id", "cluster-secret", "ca/ca.pem", "ca/ca.key.sealed",
        "tls/node-cert.pem", "tls/node-key.pem",
    ]
    return {f: m.succeed(f"sha256sum /persist/expanse/{f}").split()[0] for f in files}


form("clustercfg")

with subtest("real desired state: a file resource on n1"):
    apply_file(n1, "file:/etc/gen-base", "/etc/gen-base", "alpha")
    n1.wait_until_succeeds("grep -qx alpha /etc/gen-base", timeout=60)

with subtest("garage and restic are up on n1"):
    n1.wait_for_unit("garage.service")
    n1.wait_for_open_port(3900)
    n1.wait_for_open_port(3901)

with subtest("bootstrap a single-node garage layout, S3 key and bucket"):
    node_id = n1.succeed(
        "garage status | awk 'NR>2 && NF {print $1; exit}'"
    ).strip()
    assert node_id, "no node id found in `garage status`"
    n1.succeed(f"garage layout assign -z dc1 -c 1G {node_id}")
    n1.succeed("garage layout apply --version 1")
    key_out = n1.succeed("garage key create restic-key")
    key_id = [l for l in key_out.splitlines() if l.startswith("Key ID:")][0].split(": ", 1)[1].strip()
    secret_key = [l for l in key_out.splitlines() if l.startswith("Secret key:")][0].split(": ", 1)[1].strip()
    n1.succeed("garage bucket create backups")
    n1.succeed("garage bucket allow --read --write --key restic-key backups")

env = (
    f"AWS_ACCESS_KEY_ID={key_id} "
    f"AWS_SECRET_ACCESS_KEY={secret_key} "
    "RESTIC_PASSWORD=expanse-cluster-config-backup-password "
    "RESTIC_REPOSITORY=s3:http://127.0.0.1:3900/backups"
)

with subtest("restic init on the real garage endpoint"):
    n1.succeed(f"{env} restic init")

# --- X4: the generations store's own desired-state history ---

with subtest("X4: export the current generation and back it up"):
    n1.succeed("mkdir -p /root/backup-src")
    n1.succeed(f"expanse ctl generation export --socket {SOCK} > /root/backup-src/generation-snapshot.json")
    exported = n1.succeed("cat /root/backup-src/generation-snapshot.json")
    assert "gen-base" in exported, f"export missing the real resource key: {exported}"
    out = n1.succeed(f"{env} restic backup /root/backup-src/generation-snapshot.json")
    assert "Added to the repository: 0 B" not in out, out

with subtest("X4: the desired state is destroyed"):
    n1.succeed(f"expanse ctl resource delete file:/etc/gen-base --socket {SOCK}")
    n1.wait_until_succeeds("test ! -e /etc/gen-base", timeout=60)

with subtest("X4: restore the export from the real S3 backend and re-import it"):
    n1.succeed("rm -rf /root/restore-out")
    n1.succeed(f"{env} restic restore latest --target /root/restore-out")
    restored = n1.succeed(
        "find /root/restore-out -name generation-snapshot.json"
    ).strip().splitlines()[0]
    rc, out = n1.execute(
        f"expanse ctl generation import --socket {SOCK} --description 'restore from backup' < {restored}"
    )
    assert rc == 0, out

with subtest("X4: the reconciler recreates the file from the restored desired state"):
    n1.wait_until_succeeds("grep -qx alpha /etc/gen-base", timeout=60)

# --- X5: cluster identity material, already living under dataDir (D2) ---

with subtest("X5: capture identity checksums, then back up the whole /persist tree"):
    ident_before = identity_files(n1)
    out = n1.succeed(f"{env} restic backup /persist")
    assert "Added to the repository: 0 B" not in out, out

with subtest("X5: simulate a fresh node -- wipe the identity material entirely"):
    n1.succeed("systemctl stop expansed.service")
    n1.succeed("rm -rf /persist/expanse")
    n1.succeed("test ! -e /persist/expanse/cluster-id")

with subtest("X5: restore /persist/expanse from the real S3 backend, checksum-identical"):
    n1.succeed(f"{env} restic restore latest --target / --include /persist/expanse")
    ident_after = identity_files(n1)
    assert ident_after == ident_before, f"restored identity differs: {ident_after} != {ident_before}"

with subtest("X5: the restored node rejoins as the SAME cluster, not a fresh identity"):
    n1.succeed("systemctl start expansed.service")
    n1.wait_for_unit("expansed.service")
    wait_agent_ready(n1)
    # "3/2" (QuorumHave/QuorumNeed) is the same healthy-3-voter string
    # form() itself waits for -- 3 voters present, only 2 needed.
    wait_quorum("3/2", 120)

print("BACKUP-CLUSTER-CONFIG PASSED: generation store (X4) and cluster identity material (X5) both backed up and restored via a real restic/S3 round trip; the restored desired state reconciled for real, and the restored node rejoined the live cluster under its original identity rather than a fresh one")
