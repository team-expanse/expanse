"""Shared iSCSI initiator-side helpers for the Phase 04 iscsi/target VM
tests. Spliced into each test after cluster-common.py, client-common.py
(with `client` bound to the external initiator VM) and block-common.py.
Factored out of iscsi_target_failover.py (Stream C) so the Stream D
vertical slice reuses the same proven writer/reader logic rather than
re-implementing it.
"""

RECORD_SIZE = 512


def new_block_device(before, timeout=60):
    """The one block device name that appears under client's /dev after
    before (a set of pre-login `lsblk -ndo NAME` names) was captured."""
    deadline = time.time() + timeout
    now = set()
    while time.time() < deadline:
        now = set(client.succeed("lsblk -ndo NAME").split())
        new = now - before
        if len(new) == 1:
            return "/dev/" + next(iter(new))
        time.sleep(1)
    raise AssertionError(f"no single new block device after iscsi login: before={before} now={now}")


def start_writer(dev):
    """A background loop on the initiator writing sequential fixed-size
    records at increasing LBAs. Each record is its own dd, so a write
    lost to a broken session mid-failover is simply retried on the next
    tick -- the loop never exits on error, matching "recovers" (bounded
    retry, not zero interruption), the same shape share_smb_failover.py's
    own writer uses for a CIFS append instead of a raw LBA write."""
    client.succeed("rm -f /root/last_acked")
    client.succeed(
        "cat > /root/writer.sh << 'EOF'\n"
        "#!/bin/sh\n"
        # systemd-run's default $PATH is minimal (no coreutils) on
        # NixOS -- share_smb_failover.py's own note applies here too.
        "export PATH=/run/current-system/sw/bin:$PATH\n"
        "i=0\n"
        "while true; do\n"
        "  i=$((i+1))\n"
        f"  if printf 'seq-%08d' \"$i\" | dd of={dev} bs={RECORD_SIZE} seek=$i count=1 "
        "oflag=direct conv=notrunc 2>/dev/null; then\n"
        "    echo \"$i\" > /root/last_acked\n"
        "  fi\n"
        "  sleep 0.3\n"
        "done\n"
        "EOF\n"
    )
    client.succeed("systemd-run --unit=iscsi-writer /bin/sh /root/writer.sh")


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


def read_records(m, dev, n):
    """The first n RECORD_SIZE-byte records at LBAs 1..n, read in one dd
    (not one dd per record -- n can be in the hundreds). base64 is
    already imported by block-common.py (deploy()'s own manifest
    encoding), spliced ahead of this file."""
    out = m.succeed(f"dd if={dev} bs={RECORD_SIZE} skip=1 count={n} iflag=direct 2>/dev/null | base64 -w0")
    raw = base64.b64decode(out)
    return [raw[i * RECORD_SIZE:(i + 1) * RECORD_SIZE] for i in range(n)]


def verify_records(m, dev, n, what):
    for idx, rec in enumerate(read_records(m, dev, n), start=1):
        want = f"seq-{idx:08d}".encode()
        got = rec[:len(want)]
        assert got == want, f"{what}: record {idx} lost or corrupted: want {want!r}, got {got!r}"


def vip_holders(vip_addr, machines):
    """Nodes currently carrying vip_addr on eth1, among `machines`. Once
    a node has been crash()ed, callers MUST pass only the live survivors
    -- share_smb_failover.py's own vip_holders docstring found the same
    reconnect-silently-reboots-it gotcha querying a just-killed holder."""
    holders = []
    for m in machines:
        rc, out = m.execute(f"ip -4 -o addr show eth1 | grep -F {vip_addr} || true")
        if rc == 0 and out.strip():
            holders.append(m.name)
    return holders
