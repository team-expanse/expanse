"""backup-persist: PHASE-08-TASKS.md Stream B (X3). btrfs-backed durable
node/cluster state under /persist (D2: node identity, ssh host key, and the
impermanence bind-mount sources -- machine-id, var/lib/nixos, var/lib/systemd,
root/.ssh) backed up and restored via restic against a real in-VM garage S3
endpoint, the same rigor X1 applied: dedupe, encryption, and restore all
verified checksum-equal against real content, not a synthetic stand-in.

Runs as a single-node nixosTest's testScript; `machine` and `subtest` come
from the driver context.
"""

marker = "THE-EXPANSE-PERSIST-BACKUP-PLAINTEXT-MARKER"

machine.wait_for_unit("multi-user.target")
machine.wait_for_unit("expanse-firstboot.service")
machine.wait_for_unit("garage.service")
machine.wait_for_open_port(3900)
machine.wait_for_open_port(3901)


def restored(basename):
    return machine.succeed(f"find /root/persist-restore -name {basename}").strip().splitlines()[0]


with subtest("real /persist content exists before any backup"):
    machine.succeed(f"echo {marker} > /persist/backup-test-marker.txt")
    node_id_before = machine.succeed("cat /persist/expanse/identity/node-id").strip()
    sshkey_before = machine.succeed("sha256sum /persist/ssh/ssh_host_ed25519_key").split()[0]
    machineid_before = machine.succeed("sha256sum /persist/etc/machine-id").split()[0]

with subtest("bootstrap a single-node garage layout, S3 key and bucket"):
    node_id = machine.succeed(
        "garage status | awk 'NR>2 && NF {print $1; exit}'"
    ).strip()
    assert node_id, "no node id found in `garage status`"
    machine.succeed(f"garage layout assign -z dc1 -c 1G {node_id}")
    machine.succeed("garage layout apply --version 1")
    key_out = machine.succeed("garage key create restic-key")
    key_id = [l for l in key_out.splitlines() if l.startswith("Key ID:")][0].split(": ", 1)[1].strip()
    secret_key = [l for l in key_out.splitlines() if l.startswith("Secret key:")][0].split(": ", 1)[1].strip()
    machine.succeed("garage bucket create backups")
    machine.succeed("garage bucket allow --read --write --key restic-key backups")

env = (
    f"AWS_ACCESS_KEY_ID={key_id} "
    f"AWS_SECRET_ACCESS_KEY={secret_key} "
    "RESTIC_PASSWORD=expanse-persist-backup-password "
    "RESTIC_REPOSITORY=s3:http://127.0.0.1:3900/backups"
)

with subtest("restic init, then a real backup of the whole /persist tree"):
    machine.succeed(f"{env} restic init")
    out = machine.succeed(f"{env} restic backup /persist")
    assert "Added to the repository: 0 B" not in out, out

with subtest("second, unchanged backup of /persist dedupes to zero new bytes"):
    out = machine.succeed(f"{env} restic backup /persist")
    assert "Added to the repository: 0 B" in out, f"dedupe did not kick in: {out}"

with subtest("restore-integrity verification against the real backend"):
    machine.succeed(f"{env} restic check --read-data-subset=100%")

with subtest("restore into a separate target; content checksum-equal to /persist"):
    machine.succeed("rm -rf /root/persist-restore")
    machine.succeed(f"{env} restic restore latest --target /root/persist-restore")

    node_id_after = machine.succeed(f"cat {restored('node-id')}").strip()
    assert node_id_after == node_id_before, "restored node-id does not match"

    sshkey_after = machine.succeed(f"sha256sum {restored('ssh_host_ed25519_key')}").split()[0]
    assert sshkey_after == sshkey_before, "restored ssh host key does not match"

    machineid_after = machine.succeed(f"sha256sum {restored('machine-id')}").split()[0]
    assert machineid_after == machineid_before, "restored machine-id does not match"

    marker_after = machine.succeed(f"cat {restored('backup-test-marker.txt')}").strip()
    assert marker in marker_after, "restored marker file content does not match"

with subtest("data is actually encrypted at rest, not just declared"):
    status, out = machine.execute(
        f"grep -rl {marker} /var/lib/garage/data /var/lib/garage/meta"
    )
    # grep exit 1 == no match anywhere (encrypted); 0 == found the
    # plaintext (bad); anything else is a real error, not a pass.
    assert status == 1, f"plaintext marker found (status {status}): {out}"

print("BACKUP-PERSIST PASSED: /persist backed up and restored via restic against a real S3 endpoint, dedupe/encryption/restore-integrity all exercised for real, real node identity and ssh host key checksum-equal after restore")
