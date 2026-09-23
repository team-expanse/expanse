"""PHASE-03-TASKS.md Stream B2 (X2, the phase's decider for SMB): a
SINGLETON share/smb block survives losing the node serving it. A client
writes continuously through its CIFS mount, the serving node is hard-
killed, and the whole block -- process, volume primary and VIP alike --
reschedules to a survivor (the "slow path": a SINGLETON share has exactly
one replica to move, not just a VIP handover behind an already-running
backend, per D1's ctdb-free choice). The client's own reconnect (same VIP
address, a new node behind it) resumes writes without a manual remount.
A second, independent reader -- the host-level volume mount, bypassing
SMB entirely -- must see exactly what the client's own checksum shows.

Runs after cluster-common.py, client-common.py (with `client` bound to
the external VM), block-common.py and vol_cluster.py. Expects VIP_POOL
(two addresses, share-smb-failover.nix) spliced in ahead of this file.
"""

PORT = 445  # client-facing, VIP-exposed
SMBD_PORT = 44445  # smbd's own internal listen port (share_smb.py's collision note)
SHARE = "testshare"
MOUNT = "/mnt/share-data"
VOL_NAME = "blk-default-share-share-data"  # storage.BlockVolumeName(ns, block, storageName)
STREAM_FILE = "/mnt/client/stream.txt"
MIN_ACKS_BEFORE_KILL = 5
MIN_ACKS_AFTER_RECOVERY = 5

MANIFEST = f"""apiVersion: expanse.io/v1
kind: Block
metadata:
  name: share
  namespace: default
spec:
  type: share/smb
  replicas: 1
  strategy:
    kind: SINGLETON
  resources:
    requests:
      cpu: 100m
      memory: 128Mi
  storage:
    - name: share-data
      size: 64Mi
      replication: 3
      mountPath: {MOUNT}
  config:
    port: {SMBD_PORT}
    shareName: {SHARE}
    guestOk: true
  network:
    ports:
      - name: smb
        port: {PORT}
        target_port: {SMBD_PORT}
        protocol: tcp
        expose: EXPOSE_VIP
    health_check:
      readiness:
        type: PROBE_TCP
        port: {SMBD_PORT}
        period_seconds: 2
"""

ALL_MACHINES = {"n1": n1, "n2": n2, "n3": n3}


def vip_holders(vip_addr, machines=None):
    """Nodes currently carrying vip_addr on eth1, among `machines` (all
    three by default). Once a node has been crash()ed, callers MUST pass
    only the live survivors here: the test driver's execute() reconnects
    via connect() -> start() on any machine it isn't already connected to
    -- including a "crashed" one -- silently rebooting it from its
    persistent disk overlay and letting it briefly re-announce stale VIP
    state from before the crash (reproduced directly: a query against the
    just-killed holder made it reappear as a VIP holder on every poll)."""
    machines = machines if machines is not None else [n1, n2, n3]
    holders = []
    for m in machines:
        rc, out = m.execute(f"ip -4 -o addr show eth1 | grep -F {vip_addr} || true")
        if rc == 0 and out.strip():
            holders.append(m.name)
    return holders


def host_mount(m, vol_name):
    """The host-side bind-mount source for vol_name's device, or None if
    the volume isn't visible on m yet."""
    row = volume_row(m, vol_name)
    if row is None:
        return None
    return f"/var/lib/expanse/volumes/{row['id']}/mnt"


def start_writer():
    """A background loop on the client appending sequential lines to the
    mounted share. Each append is its own open/write/close, so a line
    lost to a broken connection mid-failover is simply retried on the
    next tick -- the loop never exits on error, matching "recovers"
    (bounded retry, not zero interruption) rather than assuming any
    single write during the outage succeeds."""
    client.succeed("rm -f /root/last_acked")
    client.succeed(
        "cat > /root/writer.sh << 'EOF'\n"
        "#!/bin/sh\n"
        # systemd-run's default $PATH is minimal (no coreutils) on
        # NixOS -- without this, "sleep" resolves to nothing and the
        # loop free-spins instead of pacing itself (reproduced directly:
        # "sleep: command not found" on every iteration).
        "export PATH=/run/current-system/sw/bin:$PATH\n"
        "i=0\n"
        "while true; do\n"
        "  i=$((i+1))\n"
        f"  if printf 'seq %06d\\n' \"$i\" >> {STREAM_FILE}; then\n"
        "    echo \"$i\" > /root/last_acked\n"
        "  fi\n"
        "  sleep 0.3\n"
        "done\n"
        "EOF\n"
    )
    client.succeed("systemd-run --unit=smb-writer /bin/sh /root/writer.sh")


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


form("smbfo")
wait_agent_ready(n1)
wait_agent_ready(n2)
wait_agent_ready(n3)

with subtest("the cluster's own management UI claims one pool address"):
    # Same pool-sizing note as share_smb.py (R7): the UI VIP and the
    # block's VIP share one pool, so the test discovers which address
    # went where rather than assuming.
    deadline = time.time() + 60
    ui_vip = None
    while time.time() < deadline and ui_vip is None:
        for pool_addr in VIP_POOL:
            if len(vip_holders(pool_addr)) == 1:
                ui_vip = pool_addr
                break
        if ui_vip is None:
            time.sleep(2)
    assert ui_vip, f"no VIP claimed within 60 s: {VIP_POOL}"

with subtest("deploy a SINGLETON share/smb block bound to a volume"):
    # Generous budget: the volume must be requested, placed and
    # replicated to a healthy state (P12's gate) before the block can
    # schedule at all -- a real bootstrapping chain, not a container start.
    deploy(n1, "share", MANIFEST)
    b = wait_phase(n1, "share", ["RUNNING"], 180)
    nodes = placement_nodes(b)
    assert len(nodes) == 1, f"share placed on {nodes}, want exactly 1: {b.get('status')}"
    holder = next(iter(nodes))

with subtest("all three replicas reach UpToDate"):
    # Captured before the crash, unconditionally, since holder (whichever
    # node that turns out to be) is about to die -- querying a live node
    # for the shared resource name works regardless of which one it was.
    for m in NODES:
        m.wait_until_succeeds("drbdadm status | grep -q '^vol-'", timeout=180)
    res = n1.succeed("drbdadm status | head -1 | cut -d' ' -f1").strip()
    wait_for(lambda: all(fully_replicated(m, res) for m in NODES), "every replica UpToDate")

with subtest("the block claims the other pool address as its own VIP"):
    remaining = [a for a in VIP_POOL if a != ui_vip]
    assert len(remaining) == 1, f"VIP_POOL must have exactly 2 addresses: {VIP_POOL}"
    VIP = remaining[0]
    deadline = time.time() + 60
    vip_ok = False
    while time.time() < deadline:
        hs = vip_holders(VIP)
        if len(hs) == 1 and hs[0] in nodes:
            vip_ok = True
            break
        time.sleep(2)
    assert vip_ok, f"share's VIP ({VIP}) never settled on {nodes} (last: {vip_holders(VIP)})"

with subtest("the client mounts and a continuous write loop starts"):
    client.succeed("mkdir -p /mnt/client")
    client.wait_until_succeeds(f"mount -t cifs //{VIP}/{SHARE} /mnt/client -o guest", timeout=60)
    start_writer()
    wait_acked_at_least(MIN_ACKS_BEFORE_KILL, 60, "pre-kill warmup")

with subtest("kill the holder's whole VM mid-write"):
    acked_at_kill = last_acked()
    t0 = time.time()
    ALL_MACHINES[holder].crash()

with subtest("the block, its volume primary and its VIP all re-converge on one survivor"):
    # Kill-after-settle: wait_phase's "phase in [RUNNING]" would return
    # immediately on a stale pre-crash read, so poll placement directly
    # (share_colocation.py's proven pattern for this exact scenario).
    # Placement can also flap through an intermediate candidate before
    # settling (electPrimary can briefly disagree with movePrimaryForBlock,
    # per share_colocation.py's own note) -- rather than trusting the
    # first observed placement, this waits until placement, the DRBD
    # primary and the VIP all agree on the *same* single node at once.
    survivors = {n: m for n, m in ALL_MACHINES.items() if n != holder}
    survivor = next(iter(survivors.values()))
    deadline = time.time() + 240
    new_holder = None
    last_seen = {}
    while time.time() < deadline:
        b = get_json(survivor, "share")
        cur_nodes = placement_nodes(b) if b else set()
        if len(cur_nodes) == 1 and holder not in cur_nodes:
            candidate = next(iter(cur_nodes))
            candidate_m = ALL_MACHINES[candidate]
            primary_role = role_of(candidate_m, res)
            # Only the live survivors: querying the crashed holder here
            # would silently reboot it (see vip_holders's docstring).
            vips = vip_holders(VIP, list(survivors.values()))
            last_seen = {"placement": candidate, "primary_role": primary_role, "vip_holders": vips}
            if primary_role == "Primary" and vips == [candidate]:
                new_holder = candidate
                break
        else:
            last_seen = {"placement": sorted(cur_nodes)}
        time.sleep(2)
    assert new_holder, \
        f"share, its volume primary and its VIP never agreed on one survivor within 240s: {last_seen}"
    new_holder_m = ALL_MACHINES[new_holder]
    reconverge_s = time.time() - t0
    print(f"share, primary and VIP all agree on {new_holder} after {reconverge_s:.1f}s")

with subtest("the client's writer resumes without a manual remount (X2)"):
    # No unmount/remount here on purpose: the point of X2 is that the
    # same CIFS session recovers on its own once the VIP (same address)
    # is live again behind a new smbd -- not that a fresh mount works.
    try:
        resumed_at = wait_acked_at_least(acked_at_kill + MIN_ACKS_AFTER_RECOVERY, 180,
                                          "post-failover writes to resume")
    except AssertionError:
        mnt = host_mount(new_holder_m, VOL_NAME) or "?"
        print(f"[diag] {new_holder} share dir: " +
              new_holder_m.execute(f"ls -la {mnt} 2>&1")[1])
        # Bypasses Samba entirely: is this a genuine OS/filesystem
        # permission problem for "nobody" on this node, or something
        # specific to Samba's own internal state/checks?
        print(f"[diag] {new_holder} raw unix append as nobody: " +
              new_holder_m.execute(
                  f"su -s /bin/sh nobody -c 'echo unix-probe >> {mnt}/stream.txt' 2>&1; "
                  "echo RC=$?")[1])
        print(f"[diag] {new_holder} id nobody: " +
              new_holder_m.execute("id nobody 2>&1")[1])
        print(f"[diag] {new_holder} stat stream.txt: " +
              new_holder_m.execute(f"stat {mnt}/stream.txt 2>&1")[1])
        print(f"[diag] {new_holder} getfattr stream.txt: " +
              new_holder_m.execute(f"getfattr -d -m - {mnt}/stream.txt 2>&1")[1])
        # The fully-resolved config as smbd itself parses it -- removes
        # any doubt about whether force user/guest ok/etc. actually
        # landed the way the generated smb.conf intended.
        print(f"[diag] {new_holder} testparm -s: " +
              new_holder_m.execute(
                  f"testparm -s {mnt}/.smb-state/smb.conf 2>&1")[1])
        print(f"[diag] {new_holder} smbd log file: " +
              new_holder_m.execute(f"cat {mnt}/.smb-state/log/log.* 2>&1")[1])
        # --debug-stdout may route everything to the unit's own stdout
        # instead of a log file -- pull the journal directly too, since
        # the log-file glob above can legitimately find nothing.
        print(f"[diag] {new_holder} unit journal: " +
              new_holder_m.execute(
                  "journalctl -u 'expanse-block-root@default-share-0.service' "
                  "--no-pager -n 200 2>&1")[1])
        print("[diag] client dmesg tail: " +
              client.execute("dmesg | grep -i cifs | tail -30")[1])
        raise
    print(f"writer resumed: {resumed_at} acked (was {acked_at_kill} at kill)")

with subtest("stop the writer and checksum the client's own view"):
    client.succeed("systemctl stop smb-writer 2>/dev/null || true")
    client.succeed("sync")
    client_sum = client.succeed(f"sha256sum {STREAM_FILE}").split()[0]

with subtest("a second, independent reader -- the host volume mount -- agrees byte for byte"):
    mnt = host_mount(new_holder_m, VOL_NAME)
    assert mnt, f"volume {VOL_NAME} not visible on {new_holder} yet"
    new_holder_m.wait_until_succeeds(f"mountpoint -q {mnt}", timeout=60)
    host_sum = new_holder_m.succeed(f"sha256sum {mnt}/stream.txt").split()[0]
    assert host_sum == client_sum, \
        f"checksum mismatch: client saw {client_sum}, host volume replica shows {host_sum}"

print("SHARE-SMB-FAILOVER DONE")
