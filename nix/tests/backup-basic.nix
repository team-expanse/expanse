# PHASE-08-TASKS.md Stream A (X1): tool adoption plus a basic backup/restore
# round-trip on a single file, against a real S3-compatible destination --
# not a local directory. D1 already picked restic via an ad hoc measured
# comparison against a scratch minio server; this test codifies that result
# as real, repo-committed test infrastructure using garage (nixpkgs'
# services.garage module, unlike minio needs no NIXPKGS_ALLOW_INSECURE) as
# the in-VM S3-compatible target, exercising the same three things X1 asks
# for: dedupe (a second, unchanged backup must add 0 B), encryption (the
# plaintext marker must never appear in garage's on-disk store) and
# restore-integrity verification (`restic check --read-data-subset=100%`),
# then a byte-for-byte checksum-verified restore.
{ self }:
{ pkgs, lib, ... }:
{
  name = "expanse-backup-basic";

  nodes.machine = { config, pkgs, ... }: {
    environment.systemPackages = [ pkgs.restic ];

    services.garage = {
      enable = true;
      package = pkgs.garage;
      settings = {
        replication_factor = 1;
        rpc_bind_addr = "[::]:3901";
        rpc_public_addr = "127.0.0.1:3901";
        # Test-only secret, fixed so the test is reproducible; never used
        # outside this disposable VM.
        rpc_secret = "b0aa753b23ae60c8c7baf46d0fb34cdf43806570f977dd9d81473072c72de5f0";
        s3_api = {
          s3_region = "garage";
          api_bind_addr = "127.0.0.1:3900";
          root_domain = ".s3.garage.localhost";
        };
      };
    };

    virtualisation.memorySize = 1024;
  };

  testScript = ''
    machine.wait_for_unit("multi-user.target")
    machine.wait_for_unit("garage.service")
    machine.wait_for_open_port(3900)
    machine.wait_for_open_port(3901)

    with subtest("bootstrap a single-node garage layout"):
        node_id = machine.succeed(
            "garage status | awk 'NR>2 && NF {print $1; exit}'"
        ).strip()
        assert node_id, "no node id found in `garage status`"
        machine.succeed(f"garage layout assign -z dc1 -c 1G {node_id}")
        machine.succeed("garage layout apply --version 1")

    with subtest("create an S3 key and bucket"):
        key_out = machine.succeed("garage key create restic-key")
        key_id = [l for l in key_out.splitlines() if l.startswith("Key ID:")][0].split(": ", 1)[1].strip()
        secret_key = [l for l in key_out.splitlines() if l.startswith("Secret key:")][0].split(": ", 1)[1].strip()
        machine.succeed("garage bucket create backups")
        machine.succeed("garage bucket allow --read --write --key restic-key backups")

    env = (
        f"AWS_ACCESS_KEY_ID={key_id} "
        f"AWS_SECRET_ACCESS_KEY={secret_key} "
        f"RESTIC_PASSWORD=expanse-test-repo-password "
        f"RESTIC_REPOSITORY=s3:http://127.0.0.1:3900/backups"
    )

    marker = "THE-EXPANSE-BACKUP-BASIC-PLAINTEXT-MARKER"

    with subtest("restic init against the real garage S3 endpoint"):
        machine.succeed("mkdir -p /root/src")
        machine.succeed(f"echo {marker} > /root/src/testfile.txt")
        machine.succeed("head -c 2097152 /dev/urandom >> /root/src/testfile.txt")
        machine.succeed(f"{env} restic init")

    with subtest("first backup stores real data"):
        out = machine.succeed(f"{env} restic backup /root/src")
        assert "Added to the repository: 0 B" not in out, out

    with subtest("second, unchanged backup dedupes to zero new bytes"):
        out = machine.succeed(f"{env} restic backup /root/src")
        assert "Added to the repository: 0 B" in out, f"dedupe did not kick in: {out}"

    with subtest("restore-integrity verification against the real backend"):
        machine.succeed(f"{env} restic check --read-data-subset=100%")

    with subtest("restore is byte-for-byte identical to source"):
        machine.succeed("rm -rf /root/restore-out")
        machine.succeed(f"{env} restic restore latest --target /root/restore-out")
        # Don't assume restic's target-nesting layout; discover it.
        restored = machine.succeed(
            "find /root/restore-out -name testfile.txt"
        ).strip().splitlines()[0]
        src_sum = machine.succeed("sha256sum /root/src/testfile.txt").split()[0]
        restored_sum = machine.succeed(f"sha256sum {restored}").split()[0]
        assert src_sum == restored_sum, "restored file checksum mismatch"

    with subtest("data is actually encrypted at rest, not just declared"):
        status, out = machine.execute(
            f"grep -rl {marker} /var/lib/garage/data /var/lib/garage/meta"
        )
        # grep exit 1 == no match anywhere (encrypted); 0 == found the
        # plaintext (bad); anything else is a real error, not a pass.
        assert status == 1, f"plaintext marker found (status {status}): {out}"
  '';
}
