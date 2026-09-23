"""PHASE-04-TASKS.md Stream C (X2, the phase's decider; X4): a SINGLETON
iscsi/target block survives losing the node serving it. An external
initiator writes continuously through its iSCSI session, the serving
node is hard-killed, and the whole block -- process, volume primary and
VIP alike -- reschedules to a survivor, the same "slow path" share/smb's
own X2 (PHASE-03-TASKS.md Stream B2) already proved for SINGLETON. The
initiator's own open-iscsi session recovery (not multipath path
failover, per D1's revision) resumes writes against the same portal
address without a manual re-login. A second, independent reader -- the
new primary's own /dev/drbdN, bypassing iSCSI entirely -- must see
exactly what the initiator's own read-back shows.

A second sub-test registers a SCSI-3 persistent reservation before the
kill and checks whether it is still honored after failover (X4), testing
D4's node-local-PR assumption directly rather than leaving it assumed.
Unlike X2, X4 does not gate this test's own pass/fail: D4 deliberately
did not build PR-state relocation in advance of evidence, so a real,
measured "did not survive" here is an expected, documented outcome, not
a bug in this test.

Runs after cluster-common.py, client-common.py (with `client` bound to
the external VM), block-common.py and vol_cluster.py. Expects VIP_POOL
(two addresses, iscsi-target-failover.nix) spliced in ahead of this file.
"""

PORT = 3260  # client-facing, VIP-exposed portal port
LIO_PORT = 33260  # LIO's own internal listen port (iscsi_target.py's collision note)
IQN = "iqn.2026-09.io.expanse:test-lun-failover"
INITIATOR_IQN = "iqn.2020-08.org.linux-iscsi.initiator:failover"
LUN_MIB = 64
RECORD_SIZE = 512
MIN_ACKS_BEFORE_KILL = 5
MIN_ACKS_AFTER_RECOVERY = 5
PR_KEY = "0xabc123"

MANIFEST = f"""apiVersion: expanse.io/v1
kind: Block
metadata:
  name: lun
  namespace: default
spec:
  type: iscsi/target
  replicas: 1
  strategy:
    kind: SINGLETON
  resources:
    requests:
      cpu: 100m
      memory: 128Mi
  storage:
    - name: lun
      size: {LUN_MIB}Mi
      replication: 3
      mountPath: /mnt/lun
      filesystem: none
  config:
    port: {LIO_PORT}
    iqn: {IQN}
  network:
    ports:
      - name: iscsi
        port: {PORT}
        target_port: {LIO_PORT}
        protocol: tcp
        expose: EXPOSE_VIP
    health_check:
      readiness:
        type: PROBE_TCP
        port: {LIO_PORT}
        period_seconds: 2
"""

ALL_MACHINES = {"n1": n1, "n2": n2, "n3": n3}


def vip_holders(vip_addr, machines=None):
    """Nodes currently carrying vip_addr on eth1, among `machines` (all
    three by default). Once a node has been crash()ed, callers MUST pass
    only the live survivors here -- share_smb_failover.py's own
    vip_holders docstring found the same reconnect-silently-reboots-it
    gotcha querying a just-killed holder."""
    machines = machines if machines is not None else [n1, n2, n3]
    holders = []
    for m in machines:
        rc, out = m.execute(f"ip -4 -o addr show eth1 | grep -F {vip_addr} || true")
        if rc == 0 and out.strip():
            holders.append(m.name)
    return holders


def new_block_device(before, timeout=60):
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


form("iscsifo")
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
    assert ui_vip, f"no VIP claimed within 60 s: {VIP_POOL}"

with subtest("deploy a SINGLETON iscsi/target block bound to a raw volume"):
    deploy(n1, "lun", MANIFEST)
    b = wait_phase(n1, "lun", ["RUNNING"], 180)
    nodes = placement_nodes(b)
    assert len(nodes) == 1, f"target placed on {nodes}, want exactly 1: {b.get('status')}"
    holder = next(iter(nodes))

with subtest("all three replicas reach UpToDate"):
    for m in NODES:
        m.wait_until_succeeds("drbdadm status | grep -q '^vol-'", timeout=180)
    res = n1.succeed("drbdadm status | head -1 | cut -d' ' -f1").strip()
    wait_for(lambda: all(fully_replicated(m, res) for m in NODES), "every replica UpToDate")
    # A fresh volume's very first primary election is independent of
    # where the scheduler places the block (iscsi_target.py's own
    # convergence note) -- wait for that convergence explicitly rather
    # than assuming it already matches.
    wait_for(lambda: {m.name for m in primaries(res)} == nodes,
              "DRBD primary to converge on the target's own placement", 90)

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
    assert vip_ok, f"target's VIP ({VIP}) never settled on {nodes} (last: {vip_holders(VIP)})"

with subtest("the initiator logs in and a continuous write loop starts"):
    # Static node registration, not SendTargets discovery -- A27's own
    # finding (its response embeds LIO's internal address, not the VIP).
    before = set(client.succeed("lsblk -ndo NAME").split())
    client.succeed(f"iscsiadm -m node -o new -T {IQN} -p {VIP}:{PORT}")
    client.succeed(f"iscsiadm -m node -T {IQN} -p {VIP}:{PORT} --login")
    dev = new_block_device(before)
    start_writer(dev)
    wait_acked_at_least(MIN_ACKS_BEFORE_KILL, 60, "pre-kill warmup")

with subtest("register a SCSI-3 persistent reservation before the kill (X4 setup)"):
    client.succeed(f"sg_persist --out --register --param-sark={PR_KEY} {dev}")
    keys_before = client.succeed(f"sg_persist --in --read-keys {dev}")
    assert PR_KEY[2:] in keys_before.lower(), f"PR key never registered: {keys_before!r}"
    client.succeed(f"sg_persist --out --reserve --param-rk={PR_KEY} --prout-type=1 {dev}")

with subtest("kill the holder's whole VM mid-write"):
    acked_at_kill = last_acked()
    t0 = time.time()
    ALL_MACHINES[holder].crash()

with subtest("the block, its volume primary and its VIP all re-converge on one survivor"):
    survivors = {n: m for n, m in ALL_MACHINES.items() if n != holder}
    survivor = next(iter(survivors.values()))
    deadline = time.time() + 240
    new_holder = None
    last_seen = {}
    while time.time() < deadline:
        b = get_json(survivor, "lun")
        cur_nodes = placement_nodes(b) if b else set()
        if len(cur_nodes) == 1 and holder not in cur_nodes:
            candidate = next(iter(cur_nodes))
            candidate_m = ALL_MACHINES[candidate]
            primary_role = role_of(candidate_m, res)
            vips = vip_holders(VIP, list(survivors.values()))
            last_seen = {"placement": candidate, "primary_role": primary_role, "vip_holders": vips}
            if primary_role == "Primary" and vips == [candidate]:
                new_holder = candidate
                break
        else:
            last_seen = {"placement": sorted(cur_nodes)}
        time.sleep(2)
    assert new_holder, \
        f"target, its volume primary and its VIP never agreed on one survivor within 240s: {last_seen}"
    new_holder_m = ALL_MACHINES[new_holder]
    reconverge_s = time.time() - t0
    print(f"target, primary and VIP all agree on {new_holder} after {reconverge_s:.1f}s")

with subtest("the initiator's writer resumes without a manual re-login (X2)"):
    # No explicit --login here on purpose: X2 is that the same iSCSI
    # session recovers on its own (open-iscsi's built-in reinstatement,
    # against the same VIP:PORT once it is live again behind a new LIO
    # instance) -- not that a fresh login works.
    resumed_at = wait_acked_at_least(acked_at_kill + MIN_ACKS_AFTER_RECOVERY, 180,
                                      "post-failover writes to resume")
    stall_s = time.time() - t0
    print(f"writer resumed: {resumed_at} acked (was {acked_at_kill} at kill); "
          f"measured stall {stall_s:.1f}s (informs D6)")

with subtest("stop the writer and every acked record checksums correctly (X2)"):
    client.succeed("systemctl stop iscsi-writer 2>/dev/null || true")
    final_acked = last_acked()
    verify_records(client, dev, final_acked, "initiator's own read-back")

with subtest("a second, independent reader -- the new primary's own device -- agrees"):
    survivor_dev = device_of(new_holder_m)
    verify_records(new_holder_m, survivor_dev, final_acked, f"survivor {new_holder}'s own device")

with subtest("X4: does the persistent reservation survive failover?"):
    # Not a release gate (Exit criteria, D4): D4 deliberately left PR
    # state node-local rather than relocating it onto a second volume in
    # advance of evidence. A "did not survive" here is a real, expected,
    # documented outcome -- not a bug in this test -- so it is reported
    # clearly rather than asserted true or silently ignored.
    keys_after = client.execute(f"sg_persist --in --read-keys {dev}")[1]
    survived = PR_KEY[2:] in keys_after.lower()
    if survived:
        print(f"X4: PR key {PR_KEY} SURVIVED failover -- keys now: {keys_after!r}")
    else:
        print(f"X4: PR key {PR_KEY} DID NOT survive failover (expected per D4, "
              f"PHASE-04-TASKS.md) -- keys now: {keys_after!r}")

print("ISCSI-TARGET-FAILOVER DONE")
